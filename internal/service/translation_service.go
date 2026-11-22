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

// messageEvent addresses one copy of a message to one user. chat and
// translation share it so message.new and message.translated look the same.
func messageEvent(typ string, v model.MessageView, to string) Event {
	return Event{Type: typ, Kind: KindTransient, RoomID: v.RoomID, To: []string{to},
		Data: map[string]any{"room_id": v.RoomID, "message": v}}
}

// TranslationService is the body of the translate:{message_id} task (spec 5.5).
type TranslationService struct {
	store   MessageStore
	members RoomMemberStore
	tr      Translator
	mod     Moderator
	pub     Publisher
	usage   UsageStore  // nil: usage is not recorded
	pq      PolishQueue // nil: pending polishes are left for the sweep
	now     func() time.Time
	log     *slog.Logger
}

// WithUsage records each real translation call in ai_usage_daily under the sender.
func (s *TranslationService) WithUsage(u UsageStore) *TranslationService {
	s.usage = u
	return s
}

// WithPolishQueue enqueues polish:{delivery_id} for rows whose translation
// finished with a polish waiting on it.
func (s *TranslationService) WithPolishQueue(q PolishQueue) *TranslationService {
	s.pq = q
	return s
}

func NewTranslationService(store MessageStore, members RoomMemberStore, tr Translator, mod Moderator, pub Publisher, now func() time.Time, log *slog.Logger) *TranslationService {
	return &TranslationService{store: store, members: members, tr: tr, mod: mod, pub: pub, now: now, log: log}
}

// Process translates every delivery of the message that is not done yet, once
// per target language. It returns non-nil when any language failed so the
// queue retries; the daily hard cap is not a failure (retrying cannot help).
func (s *TranslationService) Process(ctx context.Context, messageID string) error {
	w, err := s.store.TranslationWork(ctx, messageID)
	if errors.Is(err, model.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("translation work: %w", err)
	}
	if len(w.Pending) == 0 {
		return nil
	}

	// Group by target language in first-seen order.
	var langs []string
	groups := map[string][]model.PendingDelivery{}
	for _, d := range w.Pending {
		if _, ok := groups[d.TargetLang]; !ok {
			langs = append(langs, d.TargetLang)
		}
		groups[d.TargetLang] = append(groups[d.TargetLang], d)
	}

	var failure error
	var changed []string // receiver ids whose row this run changed
	for _, lang := range langs {
		text, status, gerr := s.translate(ctx, w, lang)
		if gerr != nil {
			failure = gerr
		}
		var base *string
		if status == model.TranslationDone {
			base = &text
		}
		for _, d := range groups[lang] {
			ok, err := s.store.SetTranslation(ctx, d.ID, base, status, s.now())
			if err != nil {
				// Rows already set done in this run are not pending any more, so a
				// retry would never announce them: publish them before failing.
				if len(changed) > 0 {
					s.publish(ctx, w, changed)
				}
				return fmt.Errorf("set translation: %w", err)
			}
			if ok {
				changed = append(changed, d.ReceiverID)
				if status == model.TranslationDone && d.PolishStatus == model.PolishPending {
					s.enqueuePolish(ctx, d.ID)
				}
			}
		}
	}

	if len(changed) > 0 {
		s.publish(ctx, w, changed)
	}
	return failure
}

// translate runs one language. It returns the status to store and an error
// only when the queue should retry.
func (s *TranslationService) translate(ctx context.Context, w model.TranslationWork, lang string) (string, string, error) {
	out, err := s.tr.Translate(ctx, llm.TranslateRequest{Text: w.Body, SrcLang: w.SrcLang, TgtLang: lang})
	s.recordUsage(ctx, w, out)
	text, risk := out.Text, out.Risk
	switch {
	case errors.Is(err, ErrAIBudgetExceeded):
		s.log.Error("translation stopped: daily hard cap reached",
			"message_id", w.MessageID, "target_lang", lang)
		return "", model.TranslationFailed, nil
	case err != nil:
		s.log.Warn("translation failed", "message_id", w.MessageID, "target_lang", lang, "err", err)
		return "", model.TranslationFailed, fmt.Errorf("translate %s: %w", lang, err)
	}
	verdict, err := s.mod.CheckTranslated(ctx, text, lang, risk)
	if err != nil {
		s.log.Warn("translation moderation failed", "message_id", w.MessageID, "target_lang", lang, "err", err)
		return "", model.TranslationFailed, fmt.Errorf("moderate %s: %w", lang, err)
	}
	if verdict != ModPass {
		// B5 has no hidden state for deliveries; B6 decides what review/block do.
		s.log.Info("translation flagged by moderation", "message_id", w.MessageID, "target_lang", lang, "verdict", int(verdict))
	}
	return text, model.TranslationDone, nil
}

func (s *TranslationService) recordUsage(ctx context.Context, w model.TranslationWork, out AIOutcome) {
	if s.usage == nil || out.Calls == 0 {
		return
	}
	if err := s.usage.Add(context.WithoutCancel(ctx), s.now(), w.SenderID, model.UsageTranslate, out.Calls,
		out.Usage.InputTokens, out.Usage.OutputTokens, out.CostMicros); err != nil {
		s.log.Warn("translation usage write failed", "message_id", w.MessageID, "err", err)
	}
}

// enqueuePolish hands a waiting polish to the queue; a failure leaves it
// pending for the sweep.
func (s *TranslationService) enqueuePolish(ctx context.Context, deliveryID string) {
	if s.pq == nil {
		return
	}
	ectx, cancel := context.WithTimeout(ctx, enqueueTimeout)
	defer cancel()
	if err := s.pq.EnqueuePolish(ectx, deliveryID, ""); err != nil {
		s.log.Warn("polish left for the sweep: enqueue failed", "delivery_id", deliveryID, "err", err)
	}
}

// publish sends message.translated to each changed receiver still seated in the room.
func (s *TranslationService) publish(ctx context.Context, w model.TranslationWork, receivers []string) {
	views, err := s.store.ViewsOf(ctx, w.MessageID)
	if err != nil {
		if !errors.Is(err, model.ErrNotFound) {
			s.log.Warn("translation views failed", "message_id", w.MessageID, "err", err)
		}
		return
	}
	for _, uid := range receivers {
		v, ok := views.Receivers[uid]
		if !ok {
			continue
		}
		active, err := s.members.IsActiveMember(ctx, w.RoomID, uid)
		if err != nil {
			s.log.Warn("translation member check failed", "message_id", w.MessageID, "user_id", uid, "err", err)
			continue
		}
		if !active {
			continue
		}
		s.pub.Publish(ctx, messageEvent(EventMessageTranslated, v, uid))
	}
}

// inlineRunTimeout bounds one in-process translation run.
const inlineRunTimeout = 30 * time.Second

// InlinePool runs translations in this process when the queue is unavailable
// (spec 5.5). It never blocks: a full pool leaves the message pending for the sweep.
type InlinePool struct {
	sem chan struct{}
	run func(ctx context.Context, messageID string) error
	log *slog.Logger
}

func NewInlinePool(size int, run func(ctx context.Context, messageID string) error, log *slog.Logger) *InlinePool {
	if size < 1 {
		size = 1
	}
	return &InlinePool{sem: make(chan struct{}, size), run: run, log: log}
}

// TryRun starts the run in a goroutine; it returns false when the pool is full.
func (p *InlinePool) TryRun(messageID string) bool {
	select {
	case p.sem <- struct{}{}:
	default:
		return false
	}
	go func() {
		defer func() { <-p.sem }()
		defer func() {
			if r := recover(); r != nil {
				p.log.Error("inline translation panicked", "message_id", messageID, "panic", fmt.Sprint(r))
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), inlineRunTimeout)
		defer cancel()
		if err := p.run(ctx, messageID); err != nil {
			p.log.Warn("inline translation failed", "message_id", messageID, "err", err)
		}
	}()
	return true
}

// TranslateQueue enqueues translate:{message_id}; dedupe makes the task id unique per attempt.
type TranslateQueue interface {
	EnqueueTranslate(ctx context.Context, messageID, dedupe string) error
}

// PolishQueue enqueues polish:{delivery_id}; dedupe makes the task id unique per attempt.
type PolishQueue interface {
	EnqueuePolish(ctx context.Context, deliveryID, dedupe string) error
}

// Compensating sweep thresholds (spec 5.5).
const (
	sweepPendingAfter = 2 * time.Minute
	sweepFailedEvery  = 10 * time.Minute
	sweepGiveUpAfter  = 24 * time.Hour
	sweepLimit        = 200
)

// SweepService re-enqueues translations and polishes that were lost or
// failed, and gives up polishes whose translation gave up (spec 5.5).
type SweepService struct {
	store  MessageStore
	q      TranslateQueue
	pq     PolishQueue                            // nil: translations only (B5a)
	giveUp func(ctx context.Context) (int, error) // PolishService.GiveUp
	now    func() time.Time
	log    *slog.Logger
}

// WithPolish adds the polish rules to each round.
func (s *SweepService) WithPolish(pq PolishQueue, giveUp func(ctx context.Context) (int, error)) *SweepService {
	s.pq, s.giveUp = pq, giveUp
	return s
}

func NewSweepService(store MessageStore, q TranslateQueue, now func() time.Time, log *slog.Logger) *SweepService {
	return &SweepService{store: store, q: q, now: now, log: log}
}

// Sweep runs one round: give up dead polishes, re-enqueue stuck polishes,
// then stuck translations. No failure stops the round; the last error is
// returned with the number of tasks that were enqueued.
func (s *SweepService) Sweep(ctx context.Context) (int, error) {
	now := s.now()
	dedupe := "sweep-" + now.UTC().Format("200601021504")
	n := 0
	var last error
	if s.pq != nil {
		if s.giveUp != nil {
			if g, err := s.giveUp(ctx); err != nil {
				s.log.Warn("polish give-up failed", "err", err)
				last = err
			} else if g > 0 {
				s.log.Info("polish give-up", "count", g)
			}
		}
		pids, err := s.store.StuckPolishes(ctx, now, sweepPendingAfter, sweepLimit)
		if err != nil {
			s.log.Warn("stuck polishes failed", "err", err)
			last = fmt.Errorf("stuck polishes: %w", err)
		}
		for _, id := range pids {
			if err := s.pq.EnqueuePolish(ctx, id, dedupe); err != nil {
				s.log.Warn("sweep enqueue failed", "delivery_id", id, "err", err)
				last = err
				continue
			}
			n++
		}
		if len(pids) > 0 {
			s.log.Info("polish sweep", "found", len(pids))
		}
	}
	ids, err := s.store.StuckTranslations(ctx, now, sweepPendingAfter, sweepFailedEvery, sweepGiveUpAfter, sweepLimit)
	if err != nil {
		return n, fmt.Errorf("stuck translations: %w", err)
	}
	for _, id := range ids {
		if err := s.q.EnqueueTranslate(ctx, id, dedupe); err != nil {
			s.log.Warn("sweep enqueue failed", "message_id", id, "err", err)
			last = err
			continue
		}
		n++
	}
	if len(ids) > 0 {
		s.log.Info("translation sweep", "found", len(ids), "enqueued", n)
	}
	if last != nil {
		return n, fmt.Errorf("sweep enqueue: %w", last)
	}
	return n, nil
}
