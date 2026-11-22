package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/pkg/llm"
)

// Polish give-up (spec 5.5): a pending polish whose translation is still
// failed this long after the message was sent becomes fallback.
const (
	polishGiveUpAfter = 24 * time.Hour
	polishGiveUpLimit = 200
)

// PolishService is the body of the polish:{delivery_id} task (spec 5.7,
// backend TRD 10.3).
type PolishService struct {
	store   MessageStore
	members RoomMemberStore
	p       Polisher
	mod     Moderator
	usage   UsageStore
	pub     Publisher
	now     func() time.Time
	log     *slog.Logger
}

func NewPolishService(store MessageStore, members RoomMemberStore, p Polisher, mod Moderator, usage UsageStore,
	pub Publisher, now func() time.Time, log *slog.Logger) *PolishService {
	return &PolishService{store: store, members: members, p: p, mod: mod, usage: usage, pub: pub, now: now, log: log}
}

// Process polishes one delivery. Only a store error is returned (the queue
// retries); AI failures, rule violations and the budget all end in fallback.
func (s *PolishService) Process(ctx context.Context, deliveryID string) error {
	w, err := s.store.PolishWork(ctx, deliveryID)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("polish work: %w", err)
	}
	if w.PolishStatus != model.PolishPending || w.TranslationStatus != model.TranslationDone || w.BaseText == nil {
		return nil // not ours to do yet; the translation enqueues it when done
	}
	base := *w.BaseText

	text, status := s.polish(ctx, w, base)
	var polished *string
	if status == model.PolishDone {
		polished = &text
	}
	changed, err := s.store.SetPolish(ctx, w.DeliveryID, polished, status, s.now())
	if err != nil {
		return fmt.Errorf("set polish: %w", err)
	}
	if changed {
		s.publish(ctx, w.MessageID, w.RoomID, w.ReceiverID)
	}
	return nil
}

// polish returns the text and status to store.
func (s *PolishService) polish(ctx context.Context, w model.PolishWork, base string) (string, string) {
	if llm.TooShortToPolish(base) {
		return base, model.PolishDone
	}
	if w.ReceiverMBTI == "" {
		s.log.Warn("polish skipped: receiver has no mbti", "delivery_id", w.DeliveryID)
		return "", model.PolishFallback
	}
	src := w.Body
	if w.SrcLang == w.TargetLang {
		src = ""
	}
	out, err := s.p.Polish(ctx, llm.PolishRequest{Text: base, Source: src, SrcLang: w.SrcLang, TgtLang: w.TargetLang,
		MBTI: w.ReceiverMBTI, Tier: llm.TierLight})
	s.recordUsage(ctx, w, out)
	if err != nil {
		var rej *PolishRejectedError
		class := "unavailable"
		switch {
		case errors.As(err, &rej):
			class = "rejected:" + rej.Check
		case errors.Is(err, ErrAIBudgetExceeded):
			class = "budget"
		}
		s.log.Info("polish fell back", "delivery_id", w.DeliveryID, "class", class)
		return "", model.PolishFallback
	}
	verdict, err := s.mod.CheckTranslated(ctx, out.Text, w.TargetLang, out.Risk)
	if err != nil {
		s.log.Warn("polish moderation failed", "delivery_id", w.DeliveryID, "err", err)
		return "", model.PolishFallback
	}
	if verdict != ModPass {
		// B5 has no hidden state for deliveries; B6 decides what review/block do.
		s.log.Info("polish flagged by moderation", "delivery_id", w.DeliveryID, "verdict", int(verdict))
	}
	return out.Text, model.PolishDone
}

func (s *PolishService) recordUsage(ctx context.Context, w model.PolishWork, out AIOutcome) {
	if out.Calls == 0 || w.PolishBy == nil {
		return
	}
	if err := s.usage.Add(context.WithoutCancel(ctx), s.now(), *w.PolishBy, model.UsagePolish, out.Calls,
		out.Usage.InputTokens, out.Usage.OutputTokens, out.CostMicros); err != nil {
		s.log.Warn("polish usage write failed", "delivery_id", w.DeliveryID, "err", err)
	}
}

// publish sends message.polished to the receiver when still seated.
func (s *PolishService) publish(ctx context.Context, messageID, roomID, receiverID string) {
	active, err := s.members.IsActiveMember(ctx, roomID, receiverID)
	if err != nil {
		s.log.Warn("polish member check failed", "message_id", messageID, "err", err)
		return
	}
	if !active {
		return
	}
	views, err := s.store.ViewsOf(ctx, messageID)
	if err != nil {
		if !errors.Is(err, model.ErrNotFound) {
			s.log.Warn("polish views failed", "message_id", messageID, "err", err)
		}
		return
	}
	if v, ok := views.Receivers[receiverID]; ok {
		s.pub.Publish(ctx, messageEvent(EventMessagePolished, v, receiverID))
	}
}

// GiveUp sets fallback on polishes whose translation gave up (24h) and
// announces each to its receiver. It returns how many rows changed.
func (s *PolishService) GiveUp(ctx context.Context) (int, error) {
	rows, err := s.store.GiveUpPolishes(ctx, s.now(), polishGiveUpAfter, polishGiveUpLimit)
	if err != nil {
		return 0, fmt.Errorf("give up polishes: %w", err)
	}
	for _, g := range rows {
		s.publish(ctx, g.MessageID, g.RoomID, g.ReceiverID)
	}
	return len(rows), nil
}
