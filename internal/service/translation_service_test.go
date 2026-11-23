package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/pkg/llm"
)

// fakeTranslator returns "<tgt>:<text>" (or text(tgt) when set) with risk[tgt]
// (default none) unless err is set, and records every request.
type fakeTranslator struct {
	mu    sync.Mutex
	err   error
	calls []llm.TranslateRequest
	risk  map[string]string
	text  func(lang string) string
}

func (f *fakeTranslator) Translate(_ context.Context, req llm.TranslateRequest) (AIOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return AIOutcome{}, f.err
	}
	out := req.TgtLang + ":" + req.Text
	if f.text != nil {
		out = f.text(req.TgtLang)
	}
	risk := llm.RiskNone
	if r, ok := f.risk[req.TgtLang]; ok {
		risk = r
	}
	return AIOutcome{Text: out, Risk: risk, Calls: 1,
		Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}, CostMicros: 3}, nil
}

func (f *fakeTranslator) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeTranslator) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type translateEnv struct {
	*joinEnv
	repo  *repository.MessageRepo
	tr    *fakeTranslator
	svc   *TranslationService
	owner string
	room  string
}

func newTranslateEnv(t *testing.T) *translateEnv {
	t.Helper()
	je := newJoinEnv(t)
	e := &translateEnv{joinEnv: je, repo: repository.NewMessageRepo(je.pool), tr: &fakeTranslator{}}
	e.owner = je.user(t, "owner@example.com", "18_plus")
	e.room = je.roomOf(t, e.owner)
	e.svc = NewTranslationService(e.repo, repository.NewRoomMemberRepo(je.pool), e.tr, PassModerator{}, je.pub,
		func() time.Time { return je.now }, discardLog())
	return e
}

// seat creates a user and seats them in the owner's room.
func (e *translateEnv) seat(t *testing.T, email string, seatNo int) string {
	t.Helper()
	id := e.user(t, email, "18_plus")
	e.exec(t, `insert into room_members(room_id, user_id, seat_no, role) values($1,$2,$3,'member')`, e.room, id, seatNo)
	return id
}

// message stores a zh text message from the owner with one pending delivery per receiver.
func (e *translateEnv) message(t *testing.T, langs map[string]string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(bg, `select gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	m := model.NewMessage{ID: id, RoomID: e.room, SenderID: e.owner, Scope: model.ScopeRoom, Kind: model.KindText,
		SrcLang: "zh-Hans", Body: "你好", ClientMsgID: "c-" + id}
	if ok, err := e.repo.InsertMessage(bg, m); err != nil || !ok {
		t.Fatal(ok, err)
	}
	var ds []model.NewDelivery
	for user, lang := range langs {
		ds = append(ds, model.NewDelivery{ReceiverID: user, TargetLang: lang,
			TranslationStatus: model.TranslationPending, PolishStatus: model.PolishNone})
	}
	if err := e.repo.InsertDeliveries(bg, id, ds); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *translateEnv) status(t *testing.T, msgID, receiver string) (string, *string) {
	t.Helper()
	var st string
	var base *string
	if err := e.pool.QueryRow(bg, `select translation_status, base_text from deliveries where message_id=$1 and receiver_id=$2`,
		msgID, receiver).Scan(&st, &base); err != nil {
		t.Fatal(err)
	}
	return st, base
}

// translatedTo returns the message.translated events addressed to user.
func (e *translateEnv) translatedTo(user string) []model.MessageView {
	var out []model.MessageView
	for _, ev := range e.pub.ofType(EventMessageTranslated) {
		if len(ev.To) == 1 && ev.To[0] == user {
			out = append(out, ev.Data["message"].(model.MessageView))
		}
	}
	return out
}

func TestTranslate_GroupsByLanguage(t *testing.T) {
	e := newTranslateEnv(t)
	k1, k2, j := e.seat(t, "k1@example.com", 2), e.seat(t, "k2@example.com", 3), e.seat(t, "j@example.com", 4)
	msg := e.message(t, map[string]string{k1: "ko", k2: "ko", j: "ja"})

	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	if n := e.tr.callCount(); n != 2 {
		t.Fatalf("translator calls = %d, want 2", n)
	}
	for _, c := range e.tr.calls {
		if c.Text != "你好" || c.SrcLang != "zh-Hans" {
			t.Fatalf("request %+v", c)
		}
	}
	for user, lang := range map[string]string{k1: "ko", k2: "ko", j: "ja"} {
		st, base := e.status(t, msg, user)
		if st != model.TranslationDone || base == nil || *base != lang+":你好" {
			t.Fatalf("%s: status=%s base=%v", lang, st, base)
		}
		evs := e.translatedTo(user)
		if len(evs) != 1 {
			t.Fatalf("%s: %d events", lang, len(evs))
		}
		v := evs[0]
		if v.Delivery == nil || v.Delivery.BaseText == nil || *v.Delivery.BaseText != lang+":你好" ||
			v.Delivery.TranslationStatus != model.TranslationDone || v.ID != msg {
			t.Fatalf("%s: view %+v", lang, v)
		}
	}
	for _, ev := range e.pub.ofType(EventMessageTranslated) {
		if ev.Kind != KindTransient || ev.RoomID != e.room || ev.Data["room_id"] != e.room {
			t.Fatalf("event %+v", ev)
		}
	}
	if n := len(e.pub.all()); n != 3 {
		t.Fatalf("events = %d, want 3", n)
	}
}

// failingSetStore fails SetTranslation from call number failAt on.
type failingSetStore struct {
	MessageStore
	mu     sync.Mutex
	calls  int
	failAt int
}

func (f *failingSetStore) SetTranslation(ctx context.Context, id string, base *string, status, modStatus string, at time.Time) (bool, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n >= f.failAt {
		return false, errors.New("db down")
	}
	return f.MessageStore.SetTranslation(ctx, id, base, status, modStatus, at)
}

// A DB error mid-loop still announces the rows this run already set done.
func TestTranslate_SetErrorStillPublishesChanged(t *testing.T) {
	e := newTranslateEnv(t)
	k1, k2 := e.seat(t, "k1@example.com", 2), e.seat(t, "k2@example.com", 3)
	msg := e.message(t, map[string]string{k1: "ko", k2: "ko"})
	e.svc.store = &failingSetStore{MessageStore: e.repo, failAt: 2}

	if err := e.svc.Process(bg, msg); err == nil {
		t.Fatal("want error from SetTranslation")
	}
	done := 0
	for _, u := range []string{k1, k2} {
		st, _ := e.status(t, msg, u)
		evs := e.translatedTo(u)
		switch st {
		case model.TranslationDone:
			done++
			if len(evs) != 1 {
				t.Fatalf("done receiver got %d events, want 1", len(evs))
			}
		case model.TranslationPending:
			if len(evs) != 0 {
				t.Fatalf("pending receiver got %d events", len(evs))
			}
		default:
			t.Fatalf("status %s", st)
		}
	}
	if done != 1 {
		t.Fatalf("done rows = %d, want 1", done)
	}
}

func TestTranslate_FailureMarksFailedAndRetries(t *testing.T) {
	e := newTranslateEnv(t)
	k := e.seat(t, "k@example.com", 2)
	msg := e.message(t, map[string]string{k: "ko"})

	e.tr.setErr(ErrAIUnavailable)
	if err := e.svc.Process(bg, msg); err == nil {
		t.Fatal("want error so asynq retries")
	}
	if st, base := e.status(t, msg, k); st != model.TranslationFailed || base != nil {
		t.Fatalf("status=%s base=%v", st, base)
	}
	evs := e.translatedTo(k)
	if len(evs) != 1 || evs[0].Delivery.TranslationStatus != model.TranslationFailed {
		t.Fatalf("events %+v", evs)
	}

	e.tr.setErr(nil)
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.status(t, msg, k); st != model.TranslationDone {
		t.Fatalf("status=%s", st)
	}
	evs = e.translatedTo(k)
	if len(evs) != 2 || evs[1].Delivery.TranslationStatus != model.TranslationDone {
		t.Fatalf("events %+v", evs)
	}
}

func TestTranslate_HardCapIsNotRetried(t *testing.T) {
	e := newTranslateEnv(t)
	k := e.seat(t, "k@example.com", 2)
	msg := e.message(t, map[string]string{k: "ko"})

	var logs strings.Builder
	e.svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	e.tr.setErr(ErrAIBudgetExceeded)
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if st, _ := e.status(t, msg, k); st != model.TranslationFailed {
		t.Fatalf("status=%s", st)
	}
	if out := logs.String(); !strings.Contains(out, "level=ERROR") || strings.Contains(out, "你好") {
		t.Fatalf("logs %q", out)
	}
}

func TestTranslate_SkipsReceiverWhoLeft(t *testing.T) {
	e := newTranslateEnv(t)
	stay, gone := e.seat(t, "stay@example.com", 2), e.seat(t, "gone@example.com", 3)
	msg := e.message(t, map[string]string{stay: "ko", gone: "ja"})
	e.exec(t, `update room_members set left_at=now(), left_reason='left' where room_id=$1 and user_id=$2`, e.room, gone)

	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.status(t, msg, gone); st != model.TranslationDone {
		t.Fatalf("gone status=%s", st)
	}
	if n := len(e.translatedTo(gone)); n != 0 {
		t.Fatalf("gone got %d events", n)
	}
	if n := len(e.translatedTo(stay)); n != 1 {
		t.Fatalf("stay got %d events", n)
	}
}

func TestTranslate_DoneIsIdempotent(t *testing.T) {
	e := newTranslateEnv(t)
	k := e.seat(t, "k@example.com", 2)
	msg := e.message(t, map[string]string{k: "ko"})
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	e.pub.reset()
	calls := e.tr.callCount()

	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	if e.tr.callCount() != calls {
		t.Fatal("translator called again")
	}
	if n := len(e.pub.all()); n != 0 {
		t.Fatalf("events = %d", n)
	}
}

func TestTranslate_MissingMessageIsNoop(t *testing.T) {
	e := newTranslateEnv(t)
	if err := e.svc.Process(bg, "00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatal(err)
	}
	if e.tr.callCount() != 0 || len(e.pub.all()) != 0 {
		t.Fatal("work done for a missing message")
	}
}

func TestInlinePool_Full(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 2)
	done := make(chan struct{}, 2)
	var gotDeadline bool
	pool := NewInlinePool(1, func(ctx context.Context, id string) error {
		_, gotDeadline = ctx.Deadline()
		started <- id
		<-release
		done <- struct{}{}
		return nil
	}, discardLog())

	if !pool.TryRun("m1") {
		t.Fatal("first TryRun refused")
	}
	if id := <-started; id != "m1" {
		t.Fatalf("started %s", id)
	}
	if pool.TryRun("m2") {
		t.Fatal("second TryRun accepted while full")
	}
	close(release)
	<-done
	if !gotDeadline {
		t.Fatal("run context has no deadline")
	}
	// The slot frees once the run returns.
	deadline := time.Now().Add(2 * time.Second)
	for !pool.TryRun("m3") {
		if time.Now().After(deadline) {
			t.Fatal("slot never freed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-done
}

func TestInlinePool_RecoversPanic(t *testing.T) {
	done := make(chan struct{}, 1)
	calls := 0
	pool := NewInlinePool(1, func(context.Context, string) error {
		calls++
		if calls == 1 {
			panic("boom")
		}
		done <- struct{}{}
		return nil
	}, discardLog())
	pool.TryRun("m1")
	deadline := time.Now().Add(2 * time.Second)
	for !pool.TryRun("m2") {
		if time.Now().After(deadline) {
			t.Fatal("slot never freed after panic")
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-done
}

type fakeQueue struct {
	mu   sync.Mutex
	ids  []string
	keys []string
}

func (q *fakeQueue) EnqueueTranslate(_ context.Context, messageID, dedupe string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ids = append(q.ids, messageID)
	q.keys = append(q.keys, dedupe)
	return nil
}

func TestSweep_EnqueuesStuck(t *testing.T) {
	e := newTranslateEnv(t)
	k := e.seat(t, "k@example.com", 2)
	now := time.Date(2026, 11, 21, 9, 30, 45, 0, time.UTC)
	stuck := e.message(t, map[string]string{k: "ko"})
	fresh := e.message(t, map[string]string{k: "ko"})
	e.exec(t, `update deliveries set updated_at=$2 where message_id=$1`, stuck, now.Add(-3*time.Minute))
	e.exec(t, `update deliveries set updated_at=$2 where message_id=$1`, fresh, now.Add(-time.Minute))
	e.exec(t, `update messages set created_at=$1`, now.Add(-5*time.Minute))

	q := &fakeQueue{}
	sw := NewSweepService(e.repo, q, func() time.Time { return now }, discardLog())
	n, err := sw.Sweep(bg)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(q.ids) != 1 || q.ids[0] != stuck {
		t.Fatalf("ids %v", q.ids)
	}
	if !strings.HasPrefix(q.keys[0], "sweep-") || q.keys[0] != "sweep-202611210930" {
		t.Fatalf("dedupe %q", q.keys[0])
	}
}
