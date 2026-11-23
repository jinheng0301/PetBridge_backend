package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

// ReportRepo stores user reports.
type ReportRepo struct{ pool *pgxpool.Pool }

func NewReportRepo(pool *pgxpool.Pool) *ReportRepo { return &ReportRepo{pool: pool} }

// Insert returns the existing open report's id and time when an identical one is already open.
func (r *ReportRepo) Insert(ctx context.Context, in model.NewReport) (string, time.Time, error) {
	var id string
	var at time.Time
	err := conn(ctx, r.pool).QueryRow(ctx, `
		insert into reports(reporter_id, target_id, room_id, message_id, reason, note)
		values($1,$2,$3,$4,$5,$6)
		on conflict do nothing
		returning id::text, created_at`,
		in.ReporterID, in.TargetID, in.RoomID, in.MessageID, in.Reason, in.Note).Scan(&id, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.FindOpen(ctx, in.ReporterID, in.TargetID, in.MessageID)
	}
	return id, at, err
}

// FindOpen returns model.ErrNotFound when there is no open report for the key.
func (r *ReportRepo) FindOpen(ctx context.Context, reporterID, targetID string, messageID *string) (string, time.Time, error) {
	if !uuidRE.MatchString(reporterID) || !uuidRE.MatchString(targetID) || (messageID != nil && !uuidRE.MatchString(*messageID)) {
		return "", time.Time{}, model.ErrNotFound
	}
	var id string
	var at time.Time
	err := conn(ctx, r.pool).QueryRow(ctx, `
		select id::text, created_at from reports
		where reporter_id=$1 and target_id=$2
		  and coalesce(message_id, '00000000-0000-0000-0000-000000000000'::uuid) = coalesce($3::uuid, '00000000-0000-0000-0000-000000000000'::uuid)
		  and status = 'open'`, reporterID, targetID, messageID).Scan(&id, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, model.ErrNotFound
	}
	return id, at, err
}

func (r *ReportRepo) CountSince(ctx context.Context, reporterID string, since time.Time) (int, error) {
	if !uuidRE.MatchString(reporterID) {
		return 0, nil
	}
	var n int
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select count(*) from reports where reporter_id=$1 and created_at >= $2`, reporterID, since).Scan(&n)
	return n, err
}

// MessageReportable: the message is in roomID, sent by senderID, and reporterID has a delivery of it.
func (r *ReportRepo) MessageReportable(ctx context.Context, messageID, roomID, senderID, reporterID string) (bool, error) {
	for _, id := range []string{messageID, roomID, senderID, reporterID} {
		if !uuidRE.MatchString(id) {
			return false, nil
		}
	}
	var ok bool
	err := conn(ctx, r.pool).QueryRow(ctx, `
		select exists(select 1 from messages m join deliveries d on d.message_id = m.id
		where m.id=$1 and m.room_id=$2 and m.sender_id=$3 and d.receiver_id=$4)`,
		messageID, roomID, senderID, reporterID).Scan(&ok)
	return ok, err
}
