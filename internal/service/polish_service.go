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
	flags   ModFlagStore // nil: no review-queue rows are written
	blocks  BlockStore   // nil: blocks are not applied
	now     func() time.Time
	log     *slog.Logger
}

// WithModFlags records flagged polish results in the review queue (spec 5.3).
func (s *PolishService) WithModFlags(f ModFlagStore) *PolishService {
	s.flags = f
	return s
}

// WithBlocks falls back polishes between users with a block and skips
// their events (spec 6.3).
func (s *PolishService) WithBlocks(b BlockStore) *PolishService {
	s.blocks = b
	return s
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
	if w.ModStatus == model.ModBlock {
		// The copy is hidden from its receiver (spec 5.8): give the polish up
		// without calling the model and without telling anyone.
		if w.PolishStatus == model.PolishPending {
			if _, err := s.store.SetPolish(ctx, w.DeliveryID, nil, model.PolishFallback, s.now()); err != nil {
				return fmt.Errorf("set polish: %w", err)
			}
		}
		return nil
	}
	// A copy between users with a block either way is never shown (spec
	// 6.3): give it up the same way, with no model call and no event.
	blocked, err := blockedBetween(ctx, s.blocks, w.SenderID, w.ReceiverID)
	if err != nil {
		return fmt.Errorf("block check: %w", err)
	}
	if blocked {
		if w.PolishStatus == model.PolishPending {
			if _, err := s.store.SetPolish(ctx, w.DeliveryID, nil, model.PolishFallback, s.now()); err != nil {
				return fmt.Errorf("set polish: %w", err)
			}
		}
		return nil
	}
	if w.PolishStatus != model.PolishPending || w.TranslationStatus != model.TranslationDone || w.BaseText == nil {
		return nil // not ours to do yet; the translation enqueues it when done
	}
	base := *w.BaseText

	text, status, flag := s.polish(ctx, w, base)
	var polished *string
	if status == model.PolishDone {
		polished = &text
	}
	changed, err := s.store.SetPolish(ctx, w.DeliveryID, polished, status, s.now())
	if err != nil {
		return fmt.Errorf("set polish: %w", err)
	}
	if changed {
		if flag != nil {
			recordFlag(ctx, s.flags, s.log, *flag)
		}
		s.publish(ctx, w.MessageID, w.RoomID, w.ReceiverID)
	}
	return nil
}

// polish returns the text and status to store, and the review-queue row to
// write once stored (spec 5.6): a blocked result falls back to the base
// translation, a review result is kept; both keep the polished text in the flag.
func (s *PolishService) polish(ctx context.Context, w model.PolishWork, base string) (string, string, *model.ModFlag) {
	if llm.TooShortToPolish(base) {
		return base, model.PolishDone, nil
	}
	if w.ReceiverMBTI == "" {
		s.log.Warn("polish skipped: receiver has no mbti", "delivery_id", w.DeliveryID)
		return "", model.PolishFallback, nil
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
		return "", model.PolishFallback, nil
	}
	res, err := s.mod.CheckTranslated(ctx, out.Text, w.TargetLang, out.Risk)
	if err != nil {
		s.log.Warn("polish moderation failed", "delivery_id", w.DeliveryID, "err", err)
		return "", model.PolishFallback, nil
	}
	if res.Verdict == ModPass {
		return out.Text, model.PolishDone, nil
	}
	s.log.Info("polish flagged by moderation", "delivery_id", w.DeliveryID,
		"verdict", verdictStatus(res.Verdict), "source", res.Source, "terms", len(res.Terms))
	f := newFlag(model.StagePolish, res, w.SenderID)
	rejected := out.Text
	f.RoomID, f.MessageID, f.DeliveryID, f.TargetLang, f.Text = &w.RoomID, &w.MessageID, &w.DeliveryID, &w.TargetLang, &rejected
	if res.Verdict == ModBlock {
		return "", model.PolishFallback, &f
	}
	return out.Text, model.PolishDone, &f
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

// publish sends message.polished to the receiver when still seated and with
// no block either way with the sender (read from the views, as GiveUp rows
// carry no sender). A failed block check publishes nothing.
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
	v, ok := views.Receivers[receiverID]
	if !ok {
		return
	}
	blocked, err := blockedBetween(ctx, s.blocks, views.Sender.Sender.UserID, receiverID)
	if err != nil {
		s.log.Warn("polish block check failed", "message_id", messageID, "err", err)
		return
	}
	if !blocked {
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
