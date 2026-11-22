package service

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/pkg/llm"
)

var uuidLike = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-`)

// fakePolisher returns outcome/err when set, else "<text>~" with one paid call.
type fakePolisher struct {
	mu      sync.Mutex
	calls   []llm.PolishRequest
	outcome *AIOutcome
	err     error
}

func (f *fakePolisher) Polish(_ context.Context, req llm.PolishRequest) (AIOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.outcome != nil {
		return *f.outcome, f.err
	}
	if f.err != nil {
		return AIOutcome{}, f.err
	}
	return AIOutcome{Text: req.Text + "~", Risk: llm.RiskNone, Calls: 1, Usage: llm.Usage{InputTokens: 40, OutputTokens: 8}, CostMicros: 9}, nil
}

func (f *fakePolisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type recPolishQueue struct {
	mu   sync.Mutex
	ids  []string
	keys []string
	err  error
}

func (q *recPolishQueue) EnqueuePolish(_ context.Context, id, dedupe string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return q.err
	}
	q.ids = append(q.ids, id)
	q.keys = append(q.keys, dedupe)
	return nil
}

func (q *recPolishQueue) list() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.ids...)
}

type polishEnv struct {
	*translateEnv
	po    *fakePolisher
	usage *repository.AIUsageRepo
	psvc  *PolishService
}

func newPolishEnv(t *testing.T) *polishEnv {
	t.Helper()
	te := newTranslateEnv(t)
	e := &polishEnv{translateEnv: te, po: &fakePolisher{}, usage: repository.NewAIUsageRepo(te.pool)}
	e.psvc = NewPolishService(te.repo, repository.NewRoomMemberRepo(te.pool), e.po, PassModerator{}, e.usage, te.pub,
		func() time.Time { return te.now }, discardLog())
	return e
}

// polishDelivery stores a message from the owner (src lang) to receiver with
// base text, translation done and polish pending by the owner; returns the delivery id.
func (e *polishEnv) polishDelivery(t *testing.T, receiver, src, tgt, body, base string) (msgID, deliveryID string) {
	t.Helper()
	if err := e.pool.QueryRow(bg, `select gen_random_uuid()::text`).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	m := model.NewMessage{ID: msgID, RoomID: e.room, SenderID: e.owner, Scope: model.ScopeRoom, Kind: model.KindText,
		SrcLang: src, Body: body, ClientMsgID: "c-" + msgID, PolishRequested: true}
	if ok, err := e.repo.InsertMessage(bg, m); err != nil || !ok {
		t.Fatal(ok, err)
	}
	owner := e.owner
	if err := e.repo.InsertDeliveries(bg, msgID, []model.NewDelivery{{ReceiverID: receiver, TargetLang: tgt, BaseText: &base,
		TranslationStatus: model.TranslationDone, PolishStatus: model.PolishPending, PolishBy: &owner}}); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(bg, `select id::text from deliveries where message_id=$1`, msgID).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	return msgID, deliveryID
}

func (e *polishEnv) polishedTo(user string) []model.MessageView {
	var out []model.MessageView
	for _, ev := range e.pub.ofType(EventMessagePolished) {
		if len(ev.To) == 1 && ev.To[0] == user {
			out = append(out, ev.Data["message"].(model.MessageView))
		}
	}
	return out
}

func (e *polishEnv) polishRow(t *testing.T, deliveryID string) (string, *string) {
	t.Helper()
	var st string
	var txt *string
	if err := e.pool.QueryRow(bg, `select polish_status, polished_text from deliveries where id=$1`, deliveryID).Scan(&st, &txt); err != nil {
		t.Fatal(err)
	}
	return st, txt
}

func (e *polishEnv) usageRows(t *testing.T) map[string][2]int64 { // "user/kind" -> {calls, cost}
	t.Helper()
	rows, err := e.pool.Query(bg, `select user_id::text, kind, calls, cost_micro from ai_usage_daily`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][2]int64{}
	for rows.Next() {
		var u, k string
		var c, cost int64
		if err := rows.Scan(&u, &k, &c, &cost); err != nil {
			t.Fatal(err)
		}
		out[u+"/"+k] = [2]int64{c, cost}
	}
	return out
}

func TestPolish_Done(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	e.exec(t, `update users set mbti='ISTJ' where id=$1`, k)
	msg, did := e.polishDelivery(t, k, "zh-Hans", "ko", "明天见", "내일 봐요")

	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	st, txt := e.polishRow(t, did)
	if st != model.PolishDone || txt == nil || *txt != "내일 봐요~" {
		t.Fatalf("row st=%s txt=%v", st, txt)
	}
	evs := e.polishedTo(k)
	if len(evs) != 1 || evs[0].ID != msg || evs[0].Delivery.PolishStatus != model.PolishDone || *evs[0].Delivery.PolishedText != "내일 봐요~" {
		t.Fatalf("events %+v", evs)
	}
	req := e.po.calls[0]
	if req.MBTI != "ISTJ" || req.Text != "내일 봐요" || req.Source != "明天见" || req.SrcLang != "zh-Hans" || req.TgtLang != "ko" || req.Tier != llm.TierLight {
		t.Fatalf("request %+v", req)
	}
	for _, s := range []string{req.Text, req.Source, req.MBTI} {
		if uuidLike.MatchString(s) {
			t.Fatalf("request carries an id: %+v", req)
		}
	}
	if u := e.usageRows(t); u[e.owner+"/polish"] != [2]int64{1, 9} || len(u) != 1 {
		t.Fatalf("usage %v", u)
	}
}

func TestPolish_SameLanguageRewritesOriginal(t *testing.T) {
	e := newPolishEnv(t)
	z := e.seat(t, "z@example.com", 2)
	_, did := e.polishDelivery(t, z, "zh-Hans", "zh-Hans", "明天见吧", "明天见吧")
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if req := e.po.calls[0]; req.Source != "" || req.Text != "明天见吧" {
		t.Fatalf("request %+v", req)
	}
}

func TestPolish_RejectedFallsBack(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	_, did := e.polishDelivery(t, k, "zh-Hans", "ko", "明天3点见", "내일 3시에 봐요")
	e.po.outcome = &AIOutcome{Calls: 1, CostMicros: 7, Usage: llm.Usage{InputTokens: 30, OutputTokens: 5}}
	e.po.err = &PolishRejectedError{Check: llm.CheckFacts}
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if st, txt := e.polishRow(t, did); st != model.PolishFallback || txt != nil {
		t.Fatalf("st=%s txt=%v", st, txt)
	}
	if evs := e.polishedTo(k); len(evs) != 1 || evs[0].Delivery.PolishStatus != model.PolishFallback {
		t.Fatalf("events %+v", evs)
	}
	if u := e.usageRows(t); u[e.owner+"/polish"] != [2]int64{1, 7} {
		t.Fatalf("usage %v", u)
	}
}

func TestPolish_UnavailableAndBudgetFallBack(t *testing.T) {
	for _, perr := range []error{ErrAIUnavailable, ErrAIBudgetExceeded} {
		e := newPolishEnv(t)
		k := e.seat(t, "k@example.com", 2)
		_, did := e.polishDelivery(t, k, "zh-Hans", "ko", "明天见面聊", "내일 만나서 얘기해요")
		e.po.err = perr
		if err := e.psvc.Process(bg, did); err != nil {
			t.Fatalf("%v: Process = %v", perr, err)
		}
		if st, _ := e.polishRow(t, did); st != model.PolishFallback {
			t.Fatalf("%v: st=%s", perr, st)
		}
		if u := e.usageRows(t); len(u) != 0 {
			t.Fatalf("%v: usage %v", perr, u)
		}
	}
}

func TestPolish_ShortTextNoCall(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	_, did := e.polishDelivery(t, k, "ko", "zh-Hans", "좋아요!", "好的！")
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if st, txt := e.polishRow(t, did); st != model.PolishDone || txt == nil || *txt != "好的！" {
		t.Fatalf("st=%s txt=%v", st, txt)
	}
	if e.po.count() != 0 || len(e.usageRows(t)) != 0 {
		t.Fatalf("calls=%d usage=%v", e.po.count(), e.usageRows(t))
	}
	if len(e.polishedTo(k)) != 1 {
		t.Fatal("short polish not announced")
	}
}

func TestPolish_NotReadyIsNoop(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	_, did := e.polishDelivery(t, k, "zh-Hans", "ko", "明天见面聊", "내일 만나서 얘기해요")
	e.exec(t, `update deliveries set translation_status='pending', base_text=null where id=$1`, did)
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.polishRow(t, did); st != model.PolishPending || e.po.count() != 0 || len(e.pub.all()) != 0 {
		t.Fatalf("st=%s calls=%d events=%d", st, e.po.count(), len(e.pub.all()))
	}
	e.exec(t, `update deliveries set translation_status='done', base_text='내일 만나서 얘기해요' where id=$1`, did)
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if err := e.psvc.Process(bg, did); err != nil { // already done: no second call
		t.Fatal(err)
	}
	if e.po.count() != 1 {
		t.Fatalf("calls = %d, want 1", e.po.count())
	}
	if err := e.psvc.Process(bg, "00000000-0000-0000-0000-000000000000"); err != nil {
		t.Fatalf("missing delivery: %v", err)
	}
}

func TestPolish_SkipsReceiverWhoLeft(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	_, did := e.polishDelivery(t, k, "zh-Hans", "ko", "明天见面聊", "내일 만나서 얘기해요")
	e.exec(t, `update room_members set left_at=now(), left_reason='left' where user_id=$1 and room_id=$2`, k, e.room)
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.polishRow(t, did); st != model.PolishDone {
		t.Fatalf("st=%s", st)
	}
	if n := len(e.pub.all()); n != 0 {
		t.Fatalf("events = %d, want 0", n)
	}
}

func TestPolish_GiveUp(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	msg, did := e.polishDelivery(t, k, "zh-Hans", "ko", "明天见面聊", "x")
	e.exec(t, `update deliveries set translation_status='failed', base_text=null where id=$1`, did)
	e.exec(t, `update messages set created_at=$2 where id=$1`, msg, e.now.Add(-25*time.Hour))
	n, err := e.psvc.GiveUp(bg)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if evs := e.polishedTo(k); len(evs) != 1 || evs[0].Delivery.PolishStatus != model.PolishFallback {
		t.Fatalf("events %+v", evs)
	}
	if n, _ := e.psvc.GiveUp(bg); n != 0 {
		t.Fatalf("second run n=%d", n)
	}
}

func TestTranslate_EnqueuesPendingPolish(t *testing.T) {
	e := newTranslateEnv(t)
	k1, k2 := e.seat(t, "k1@example.com", 2), e.seat(t, "k2@example.com", 3)
	msg := e.message(t, map[string]string{k1: "ko", k2: "ko"})
	e.exec(t, `update deliveries set polish_status='pending', polish_by=$2 where message_id=$1 and receiver_id=$3`, msg, e.owner, k1)
	q := &recPolishQueue{}
	e.svc.WithPolishQueue(q)
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	var want string
	e.pool.QueryRow(bg, `select id::text from deliveries where message_id=$1 and receiver_id=$2`, msg, k1).Scan(&want)
	if got := q.list(); len(got) != 1 || got[0] != want {
		t.Fatalf("enqueued %v, want [%s]", got, want)
	}

	// An enqueue failure is only logged; the translation still succeeds.
	msg2 := e.message(t, map[string]string{k1: "ko"})
	e.exec(t, `update deliveries set polish_status='pending', polish_by=$2 where message_id=$1`, msg2, e.owner)
	q.err = errors.New("redis down")
	if err := e.svc.Process(bg, msg2); err != nil {
		t.Fatalf("Process = %v", err)
	}
}

func TestTranslate_RecordsUsageForSender(t *testing.T) {
	e := newTranslateEnv(t)
	usage := repository.NewAIUsageRepo(e.pool)
	e.svc.WithUsage(usage)
	k1, k2 := e.seat(t, "k1@example.com", 2), e.seat(t, "k2@example.com", 3)
	if err := e.svc.Process(bg, e.message(t, map[string]string{k1: "ko", k2: "ko"})); err != nil {
		t.Fatal(err)
	}
	var calls, cost int64
	var uid string
	if err := e.pool.QueryRow(bg, `select user_id::text, calls, cost_micro from ai_usage_daily where kind='translate'`).Scan(&uid, &calls, &cost); err != nil {
		t.Fatal(err)
	}
	if uid != e.owner || calls != 1 || cost != 3 {
		t.Fatalf("usage uid=%s calls=%d cost=%d", uid, calls, cost)
	}
}
