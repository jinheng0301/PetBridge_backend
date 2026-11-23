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
	usage   UsageStore   // nil: usage is not recorded
	pq      PolishQueue  // nil: pending polishes are left for the sweep
	flags   ModFlagStore // nil: no review-queue rows are written
	now     func() time.Time
	log     *slog.Logger
}

// WithModFlags records flagged translations in the review queue (spec 5.3).
func (s *TranslationService) WithModFlags(f ModFlagStore) *TranslationService {
	s.flags = f
	return s
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
//
// Each translation is moderated (spec 5.5): review stores it with a flag;
// block hides that language group from its receivers (message.removed instead
// of message.translated); when the model itself rated it block, the whole
// message is withdrawn from every receiver and the run stops.
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
	var changed []string // receiver ids whose row this run set translated
	var removed []string // receiver ids whose row this run hid
	var polish []string  // delivery ids whose waiting polish can start now
	// announce publishes what this run changed and hands the ready polishes to
	// the queue. Enqueueing waits for the end of the run so a later group's
	// withdrawal never leaves a polish of a withdrawn message running.
	announce := func() {
		if len(changed) > 0 {
			s.publish(ctx, w, changed)
		}
		if len(removed) > 0 {
			s.publishRemoved(ctx, w, removed)
		}
		for _, id := range polish {
			s.enqueuePolish(ctx, id)
		}
	}
	for _, lang := range langs {
		text, status, modStatus, res, gerr := s.translate(ctx, w, lang)
		if gerr != nil {
			failure = gerr
		}
		if res.Model == ModBlock {
			// The model judged the source itself unsafe: withdraw the whole
			// message. Its remaining groups are not translated, and receivers
			// this run already changed get message.removed instead.
			if err := s.withdraw(ctx, w, lang, res); err != nil {
				announce()
				return err
			}
			return nil
		}
		var base *string
		if status == model.TranslationDone {
			base = &text
		}
		for _, d := range groups[lang] {
			ok, err := s.store.SetTranslation(ctx, d.ID, base, status, modStatus, s.now())
			if err != nil {
				// Rows already set done in this run are not pending any more, so a
				// retry would never announce them: publish them before failing.
				announce()
				return fmt.Errorf("set translation: %w", err)
			}
			if !ok {
				continue
			}
			if modStatus != model.ModOK {
				f := newFlag(model.StageTranslation, res, w.SenderID)
				f.RoomID, f.MessageID, f.DeliveryID, f.TargetLang = &w.RoomID, &w.MessageID, &d.ID, &lang
				recordFlag(ctx, s.flags, s.log, f)
			}
			if modStatus == model.ModBlock {
				// Hidden from this receiver: a waiting polish will never be shown.
				// If this write fails, the sweep re-enqueues the polish and the
				// polish task falls it back without calling the model.
				removed = append(removed, d.ReceiverID)
				if d.PolishStatus == model.PolishPending {
					if _, err := s.store.SetPolish(ctx, d.ID, nil, model.PolishFallback, s.now()); err != nil {
						s.log.Warn("hidden delivery polish fallback failed", "delivery_id", d.ID, "err", err)
					}
				}
				continue
			}
			changed = append(changed, d.ReceiverID)
			if status == model.TranslationDone && d.PolishStatus == model.PolishPending {
				polish = append(polish, d.ID)
			}
		}
	}

	announce()
	return failure
}

// withdraw hides the message from every receiver (spec 5.5, model block):
// one statement marks it and all its deliveries blocked, then each receiver
// still seated gets message.removed, and one flag covers the message.
func (s *TranslationService) withdraw(ctx context.Context, w model.TranslationWork, lang string, res ModResult) error {
	receivers, err := s.store.WithdrawMessage(ctx, w.MessageID, s.now())
	if err != nil {
		return fmt.Errorf("withdraw message: %w", err)
	}
	s.log.Info("message withdrawn by moderation", "message_id", w.MessageID, "target_lang", lang, "source", res.Source)
	f := newFlag(model.StageTranslation, res, w.SenderID)
	f.RoomID, f.MessageID, f.TargetLang = &w.RoomID, &w.MessageID, &lang
	recordFlag(ctx, s.flags, s.log, f)
	s.publishRemoved(ctx, w, receivers)
	return nil
}

// translate runs one language. It returns the text, status and mod_status to
// store with the moderation result, and an error only when the queue should
// retry. Failed translations are not moderated (the receiver keeps the
// source, which passed the outgoing check).
func (s *TranslationService) translate(ctx context.Context, w model.TranslationWork, lang string) (string, string, string, ModResult, error) {
	out, err := s.tr.Translate(ctx, llm.TranslateRequest{Text: w.Body, SrcLang: w.SrcLang, TgtLang: lang})
	s.recordUsage(ctx, w, out)
	text, risk := out.Text, out.Risk
	switch {
	case errors.Is(err, ErrAIBudgetExceeded):
		s.log.Error("translation stopped: daily hard cap reached",
			"message_id", w.MessageID, "target_lang", lang)
		return "", model.TranslationFailed, model.ModOK, ModResult{}, nil
	case err != nil:
		s.log.Warn("translation failed", "message_id", w.MessageID, "target_lang", lang, "err", err)
		return "", model.TranslationFailed, model.ModOK, ModResult{}, fmt.Errorf("translate %s: %w", lang, err)
	}
	res, err := s.mod.CheckTranslated(ctx, text, lang, risk)
	if err != nil {
		s.log.Warn("translation moderation failed", "message_id", w.MessageID, "target_lang", lang, "err", err)
		return "", model.TranslationFailed, model.ModOK, ModResult{}, fmt.Errorf("moderate %s: %w", lang, err)
	}
	if res.Verdict != ModPass {
		s.log.Info("translation flagged by moderation", "message_id", w.MessageID, "target_lang", lang,
			"verdict", verdictStatus(res.Verdict), "source", res.Source, "terms", len(res.Terms))
	}
	return text, model.TranslationDone, verdictStatus(res.Verdict), res, nil
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

// publishRemoved sends message.removed to each receiver still seated in the room.
func (s *TranslationService) publishRemoved(ctx context.Context, w model.TranslationWork, receivers []string) {
	for _, uid := range receivers {
		active, err := s.members.IsActiveMember(ctx, w.RoomID, uid)
		if err != nil {
			s.log.Warn("translation member check failed", "message_id", w.MessageID, "user_id", uid, "err", err)
			continue
		}
		if active {
			s.pub.Publish(ctx, messageRemovedEvent(w.RoomID, w.MessageID, uid))
		}
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
