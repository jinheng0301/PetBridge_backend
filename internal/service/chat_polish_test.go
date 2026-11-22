package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
)

// countingLimiter wraps a real PolishGuard and counts successful takes per user.
type countingLimiter struct {
	inner *ratelimit.PolishGuard
	mu    sync.Mutex
	taken map[string]int
}

func (l *countingLimiter) Take(ctx context.Context, uid, day string, left time.Duration) (string, time.Duration) {
	r, w := l.inner.Take(ctx, uid, day, left)
	if r == "" {
		l.mu.Lock()
		l.taken[uid]++
		l.mu.Unlock()
	}
	return r, w
}

func (l *countingLimiter) n(uid string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.taken[uid]
}

type polishChatEnv struct {
	*chatEnv
	lim   *countingLimiter
	meter *gwMeter
	pq    *recPolishQueue
	clock time.Time
}

func newPolishChatEnv(t *testing.T, daily int, enabled bool) *polishChatEnv {
	t.Helper()
	ce := newChatEnv(t, 2)
	e := &polishChatEnv{chatEnv: ce, meter: newGWMeter(), pq: &recPolishQueue{}, clock: ce.now}
	e.lim = &countingLimiter{inner: ratelimit.NewPolishGuard(ratelimit.NewMemory(func() time.Time { return e.clock }), 6, daily),
		taken: map[string]int{}}
	quota := NewQuotaService(e.lim, e.meter, QuotaConfig{Enabled: enabled, DailyBudgetMicros: 1000}, func() time.Time { return e.clock })
	ce.svc.WithPolish(quota, e.pq)
	return e
}

func polishText(cmid, s string) SendInput {
	in := text(cmid, s)
	in.Polish = true
	return in
}

type polishRowState struct {
	id, status string
	by, text   *string
}

func (e *polishChatEnv) polishRows(t *testing.T, msgID string) map[string]polishRowState {
	t.Helper()
	rs, err := e.pool.Query(bg, `select receiver_id::text, id::text, polish_status, polish_by::text, polished_text from deliveries where message_id=$1`, msgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]polishRowState{}
	for rs.Next() {
		var rid string
		var r polishRowState
		if err := rs.Scan(&rid, &r.id, &r.status, &r.by, &r.text); err != nil {
			t.Fatal(err)
		}
		out[rid] = r
	}
	return out
}

// translatedDelivery stores a ko text message from sender to receiver with
// translation done and polish none; returns the delivery id.
func (e *polishChatEnv) translatedDelivery(t *testing.T, sender, receiver, base string, kind string) string {
	t.Helper()
	var id string
	e.pool.QueryRow(bg, `select gen_random_uuid()::text`).Scan(&id)
	m := model.NewMessage{ID: id, RoomID: e.room, SenderID: sender, Scope: model.ScopeRoom, Kind: kind,
		SrcLang: "ko", Body: "원문입니다", ClientMsgID: "c-" + id}
	if kind == model.KindPhrase {
		p := "thank_you"
		m.PhraseID = &p
	}
	if ok, err := e.repo.InsertMessage(bg, m); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := e.repo.InsertDeliveries(bg, id, []model.NewDelivery{{ReceiverID: receiver, TargetLang: "zh-Hans", BaseText: &base,
		TranslationStatus: model.TranslationDone, PolishStatus: model.PolishNone}}); err != nil {
		t.Fatal(err)
	}
	var did string
	e.pool.QueryRow(bg, `select id::text from deliveries where message_id=$1`, id).Scan(&did)
	return did
}

func TestSend_PolishToggle(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	z := e.seat(t, "z@example.com", "zh-Hans", "18_plus", 3)
	v, err := e.svc.Send(bg, e.owner, e.room, polishText("c1", "明天下午三点开会，记得带资料"))
	if err != nil {
		t.Fatal(err)
	}
	if !v.PolishRequested {
		t.Fatal("polish_requested = false")
	}
	rows := e.polishRows(t, v.ID)
	for _, u := range []string{k, z} {
		r := rows[u]
		if r.status != model.PolishPending || r.by == nil || *r.by != e.owner {
			t.Fatalf("%s row %+v", u, r)
		}
	}
	if got := e.pq.list(); len(got) != 1 || got[0] != rows[z].id {
		t.Fatalf("enqueued %v, want only the same-language row %s", got, rows[z].id)
	}
	if n := e.lim.n(e.owner); n != 1 {
		t.Fatalf("quota taken %d, want 1", n)
	}
}

func TestSend_PolishQuotaExhaustedStillSends(t *testing.T) {
	e := newPolishChatEnv(t, 0, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	v, err := e.svc.Send(bg, e.owner, e.room, polishText("c1", "明天下午三点开会，记得带资料"))
	if err != nil || v.PolishRequested {
		t.Fatalf("v=%+v err=%v", v, err)
	}
	if r := e.polishRows(t, v.ID)[k]; r.status != model.PolishNone || r.by != nil {
		t.Fatalf("row %+v", r)
	}
	if len(e.pq.list()) != 0 {
		t.Fatal("polish enqueued without quota")
	}
}

func TestSend_PolishShortTextFree(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	z := e.seat(t, "z@example.com", "zh-Hans", "18_plus", 3)
	v, err := e.svc.Send(bg, e.owner, e.room, polishText("c1", "OK!"))
	if err != nil || !v.PolishRequested {
		t.Fatalf("v=%+v err=%v", v, err)
	}
	if n := e.lim.n(e.owner); n != 0 {
		t.Fatalf("quota taken %d for short text", n)
	}
	rows := e.polishRows(t, v.ID)
	if r := rows[z]; r.status != model.PolishDone || r.text == nil || *r.text != "OK!" {
		t.Fatalf("same-language row %+v", r)
	}
	if r := rows[k]; r.status != model.PolishPending {
		t.Fatalf("cross-language row %+v", r)
	}
	if len(e.pq.list()) != 0 {
		t.Fatal("done row enqueued")
	}
}

func TestSend_PolishIgnoredForPhrase(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	e.seat(t, "k@example.com", "ko", "18_plus", 2)
	v, err := e.svc.Send(bg, e.owner, e.room, SendInput{ClientMsgID: "c1", Kind: model.KindPhrase, PhraseID: "thank_you", Polish: true})
	if err != nil || v.PolishRequested || e.lim.n(e.owner) != 0 {
		t.Fatalf("v=%+v err=%v taken=%d", v, err, e.lim.n(e.owner))
	}
}

func TestSend_ReplayDoesNotChargePolish(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	e.seat(t, "k@example.com", "ko", "18_plus", 2)
	v1, err := e.svc.Send(bg, e.owner, e.room, polishText("c1", "明天下午三点开会，记得带资料"))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := e.svc.Send(bg, e.owner, e.room, polishText("c1", "明天下午三点开会，记得带资料"))
	if err != nil || v2.ID != v1.ID {
		t.Fatalf("replay v=%s err=%v", v2.ID, err)
	}
	var n int
	e.pool.QueryRow(bg, `select count(*) from messages where sender_id=$1`, e.owner).Scan(&n)
	if n != 1 || e.lim.n(e.owner) != 1 {
		t.Fatalf("messages=%d taken=%d", n, e.lim.n(e.owner))
	}
}

func TestPolishButton_ChargesReceiver(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, "明天下午三点开会，记得带资料", model.KindText)
	v, err := e.svc.Polish(bg, e.owner, did)
	if err != nil {
		t.Fatal(err)
	}
	if v.Delivery == nil || v.Delivery.ID != did || v.Delivery.PolishStatus != model.PolishPending || v.ClientMsgID != nil {
		t.Fatalf("view %+v", v)
	}
	if e.lim.n(e.owner) != 1 || e.lim.n(k) != 0 || len(e.pq.list()) != 1 {
		t.Fatalf("taken receiver=%d sender=%d enqueued=%v", e.lim.n(e.owner), e.lim.n(k), e.pq.list())
	}
	v2, err := e.svc.Polish(bg, e.owner, did)
	if err != nil || v2.Delivery.PolishStatus != model.PolishPending || e.lim.n(e.owner) != 1 || len(e.pq.list()) != 1 {
		t.Fatalf("repeat v=%+v err=%v taken=%d enqueued=%v", v2.Delivery, err, e.lim.n(e.owner), e.pq.list())
	}
}

func TestPolishButton_ConcurrentTapsChargeOnce(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, "明天下午三点开会，记得带资料", model.KindText)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	views := make([]model.MessageView, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			views[i], errs[i] = e.svc.Polish(bg, e.owner, did)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		if views[i].Delivery == nil || views[i].Delivery.PolishStatus != model.PolishPending || views[i].Delivery.ID != did {
			t.Fatalf("tap %d view %+v", i, views[i].Delivery)
		}
	}
	if e.lim.n(e.owner) != 1 || len(e.pq.list()) != 1 {
		t.Fatalf("taken=%d enqueued=%v", e.lim.n(e.owner), e.pq.list())
	}
}

func TestPolishButton_NotReady(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, "明天下午三点开会", model.KindText)
	e.exec(t, `update deliveries set translation_status='pending', base_text=null where id=$1`, did)
	if _, err := e.svc.Polish(bg, e.owner, did); !errors.Is(err, ErrPolishNotReady) {
		t.Fatalf("pending translation err=%v", err)
	}
	phrase := e.translatedDelivery(t, k, e.owner, "谢谢", model.KindPhrase)
	if _, err := e.svc.Polish(bg, e.owner, phrase); !errors.Is(err, ErrPolishNotReady) {
		t.Fatalf("phrase err=%v", err)
	}
	if e.lim.n(e.owner) != 0 {
		t.Fatal("quota taken for a not-ready polish")
	}
}

func TestPolishButton_NotSeatedNotFound(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, "明天下午三点开会", model.KindText)
	if _, err := e.svc.Polish(bg, k, did); !errors.Is(err, ErrDeliveryNotFound) { // the sender is not the receiver
		t.Fatalf("other's delivery err=%v", err)
	}
	if _, err := e.svc.Polish(bg, e.owner, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("random id err=%v", err)
	}
	other := e.translatedDelivery(t, e.owner, k, "내일 오후 세 시에 회의해요", model.KindText)
	e.exec(t, `update room_members set left_at=now(), left_reason='left' where room_id=$1 and user_id=$2`, e.room, k)
	if _, err := e.svc.Polish(bg, k, other); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("left receiver err=%v", err)
	}
}

func TestPolishButton_QuotaReasons(t *testing.T) {
	reason := func(_ model.MessageView, err error) string {
		var qe *QuotaError
		if !errors.As(err, &qe) {
			t.Fatalf("err = %v, want *QuotaError", err)
		}
		return qe.Reason
	}

	e := newPolishChatEnv(t, 0, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, "明天下午三点开会，记得带资料", model.KindText)
	if r := reason(e.svc.Polish(bg, e.owner, did)); r != QuotaDailyQuota {
		t.Fatalf("reason %s", r)
	}
	var st string
	var by *string
	e.pool.QueryRow(bg, `select polish_status, polish_by::text from deliveries where id=$1`, did).Scan(&st, &by)
	if st != model.PolishNone || by != nil {
		t.Fatalf("claim not released: %s %v", st, by)
	}

	e = newPolishChatEnv(t, 30, false)
	k = e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did = e.translatedDelivery(t, k, e.owner, "明天下午三点开会，记得带资料", model.KindText)
	if r := reason(e.svc.Polish(bg, e.owner, did)); r != QuotaDisabled {
		t.Fatalf("reason %s", r)
	}

	e = newPolishChatEnv(t, 30, true)
	k = e.seat(t, "k@example.com", "ko", "18_plus", 2)
	e.meter.data[e.clock.UTC().Format("20060102")] = 1000
	did = e.translatedDelivery(t, k, e.owner, "明天下午三点开会，记得带资料", model.KindText)
	if r := reason(e.svc.Polish(bg, e.owner, did)); r != QuotaGlobalBudget {
		t.Fatalf("reason %s", r)
	}

	e = newPolishChatEnv(t, 30, true)
	k = e.seat(t, "k@example.com", "ko", "18_plus", 2)
	for i := 0; i < 6; i++ {
		d := e.translatedDelivery(t, k, e.owner, "明天下午三点开会，记得带资料", model.KindText)
		if _, err := e.svc.Polish(bg, e.owner, d); err != nil {
			t.Fatalf("polish %d: %v", i+1, err)
		}
	}
	d := e.translatedDelivery(t, k, e.owner, "明天下午三点开会，记得带资料", model.KindText)
	_, err := e.svc.Polish(bg, e.owner, d)
	var qe *QuotaError
	if !errors.As(err, &qe) || qe.Reason != QuotaMinuteLimit || qe.RetryAfter <= 0 {
		t.Fatalf("7th err=%v", err)
	}
}

func TestPolishButton_ShortTextNoCharge(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, "好的", model.KindText)
	v, err := e.svc.Polish(bg, e.owner, did)
	if err != nil || v.Delivery.PolishStatus != model.PolishDone || v.Delivery.PolishedText == nil || *v.Delivery.PolishedText != "好的" {
		t.Fatalf("v=%+v err=%v", v.Delivery, err)
	}
	if e.lim.n(e.owner) != 0 || len(e.pq.list()) != 0 {
		t.Fatalf("taken=%d enqueued=%v", e.lim.n(e.owner), e.pq.list())
	}
}

func TestPolishButton_EnqueueFailsLeavesPending(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	e.pq.err = errors.New("redis down")
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, "明天下午三点开会，记得带资料", model.KindText)
	v, err := e.svc.Polish(bg, e.owner, did)
	if err != nil || v.Delivery.PolishStatus != model.PolishPending {
		t.Fatalf("v=%+v err=%v", v.Delivery, err)
	}
}

// gateLimiter blocks its first Take until release is closed, then refuses
// every take with daily_quota.
type gateLimiter struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (l *gateLimiter) Take(context.Context, string, string, time.Duration) (string, time.Duration) {
	l.mu.Lock()
	l.calls++
	first := l.calls == 1
	l.mu.Unlock()
	if first {
		close(l.entered)
		<-l.release
	}
	return QuotaDailyQuota, 0
}

// A tap that arrives while another tap's charge is being refused must not
// report pending: the row goes back to none and no message.polished follows.
func TestPolishButton_ConcurrentTapRefusedNeverReportsPending(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	lim := &gateLimiter{entered: make(chan struct{}), release: make(chan struct{})}
	e.svc.WithPolish(NewQuotaService(lim, e.meter, QuotaConfig{Enabled: true, DailyBudgetMicros: 1000},
		func() time.Time { return e.clock }), e.pq)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, "明天下午三点开会，记得带资料", model.KindText)

	errA := make(chan error, 1)
	go func() { _, err := e.svc.Polish(bg, e.owner, did); errA <- err }()
	<-lim.entered // tap A holds the claim and waits inside its charge

	type res struct {
		v   model.MessageView
		err error
	}
	resB := make(chan res, 1)
	go func() { v, err := e.svc.Polish(bg, e.owner, did); resB <- res{v, err} }()
	time.Sleep(300 * time.Millisecond) // let tap B reach the row
	close(lim.release)

	if err := <-errA; !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("tap A err=%v", err)
	}
	b := <-resB
	if b.err == nil {
		t.Fatalf("tap B returned %s while the row went back to none", b.v.Delivery.PolishStatus)
	}
	if !errors.Is(b.err, ErrQuotaExceeded) {
		t.Fatalf("tap B err=%v", b.err)
	}
	var st string
	e.pool.QueryRow(bg, `select polish_status from deliveries where id=$1`, did).Scan(&st)
	if st != model.PolishNone || len(e.pq.list()) != 0 {
		t.Fatalf("row %s enqueued %v", st, e.pq.list())
	}
}
