package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jinheng0301/petbridge/internal/model"
)

const (
	// reportDailyLimit caps reports per user per UTC day (spec 7).
	reportDailyLimit = 20
	reportNoteMax    = 500
)

// ReportInput is a report as submitted. MessageID and Note may be empty.
type ReportInput struct {
	TargetUserID, RoomID, MessageID, Reason, Note string
}

// ReportResult identifies the stored (or already open) report.
type ReportResult struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// ReportService stores user reports. It takes no automatic action: reports are
// handled later by an operator (spec 7).
type ReportService struct {
	users   UserStore
	members RoomMemberStore
	reports ReportStore
	now     func() time.Time
	log     *slog.Logger
}

func NewReportService(users UserStore, members RoomMemberStore, reports ReportStore, now func() time.Time, log *slog.Logger) *ReportService {
	return &ReportService{users: users, members: members, reports: reports, now: now, log: log}
}

// Report files a report from userID. An identical open report is returned
// as-is without counting against the daily limit.
func (s *ReportService) Report(ctx context.Context, userID string, in ReportInput) (ReportResult, error) {
	_, onboarded, err := s.users.State(ctx, userID)
	if errors.Is(err, model.ErrNotFound) {
		return ReportResult{}, ErrUserNotFound
	}
	if err != nil {
		return ReportResult{}, fmt.Errorf("user state: %w", err)
	}
	if !onboarded {
		return ReportResult{}, ErrOnboardingRequired
	}

	target := strings.ToLower(in.TargetUserID)
	room := strings.ToLower(in.RoomID)
	msg := strings.ToLower(in.MessageID)
	note := strings.TrimSpace(in.Note)
	var bad []string
	if !isUUID(target) || target == userID {
		bad = append(bad, "target_user_id")
	}
	if !isUUID(room) {
		bad = append(bad, "room_id")
	}
	if !slices.Contains(model.ReportReasons, in.Reason) {
		bad = append(bad, "reason")
	}
	if utf8.RuneCountInString(note) > reportNoteMax {
		bad = append(bad, "note")
	}
	if msg != "" && !isUUID(msg) {
		bad = append(bad, "message_id")
	}
	if len(bad) > 0 {
		return ReportResult{}, &ValidationError{Fields: bad}
	}

	for _, id := range []string{userID, target} {
		ever, err := s.members.EverMember(ctx, room, id)
		if err != nil {
			return ReportResult{}, fmt.Errorf("ever member: %w", err)
		}
		if !ever {
			return ReportResult{}, &ValidationError{Fields: []string{"room_id"}}
		}
	}

	var msgPtr, notePtr *string
	if msg != "" {
		ok, err := s.reports.MessageReportable(ctx, msg, room, target, userID)
		if err != nil {
			return ReportResult{}, fmt.Errorf("message reportable: %w", err)
		}
		if !ok {
			return ReportResult{}, &ValidationError{Fields: []string{"message_id"}}
		}
		msgPtr = &msg
	}
	if note != "" {
		notePtr = &note
	}

	// An identical open report is returned before the limit is checked.
	id, createdAt, err := s.reports.FindOpen(ctx, userID, target, msgPtr)
	if err == nil {
		return ReportResult{ID: id, CreatedAt: createdAt}, nil
	}
	if !errors.Is(err, model.ErrNotFound) {
		return ReportResult{}, fmt.Errorf("find open report: %w", err)
	}

	now := s.now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	n, err := s.reports.CountSince(ctx, userID, dayStart)
	if err != nil {
		return ReportResult{}, fmt.Errorf("count reports: %w", err)
	}
	if n >= reportDailyLimit {
		return ReportResult{}, &RateLimitedError{RetryAfter: dayStart.AddDate(0, 0, 1).Sub(now)}
	}

	id, createdAt, err = s.reports.Insert(ctx, model.NewReport{
		ReporterID: userID, TargetID: target, RoomID: room, MessageID: msgPtr, Reason: in.Reason, Note: notePtr,
	})
	if err != nil {
		return ReportResult{}, fmt.Errorf("insert report: %w", err)
	}
	if s.priority(ctx, in.Reason, userID, target) {
		s.log.Warn("report needs priority review", "report_id", id, "reason", in.Reason)
	}
	return ReportResult{ID: id, CreatedAt: createdAt}, nil
}

// priority reports whether the report needs prompt attention: minor safety, or
// either party being a 13-17 user. A profile read failure only costs the flag.
func (s *ReportService) priority(ctx context.Context, reason string, ids ...string) bool {
	if reason == "minor_safety" {
		return true
	}
	for _, id := range ids {
		p, _, err := s.users.LoadProfile(ctx, id, false)
		if err != nil {
			s.log.Warn("report priority check failed", "user_id", id, "err", err)
			continue
		}
		if p.AgeBand != nil && *p.AgeBand == "13_17" {
			return true
		}
	}
	return false
}
