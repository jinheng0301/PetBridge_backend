package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/repository"
)

func newBlockService(e *joinEnv) *BlockService {
	return NewBlockService(repository.NewUserRepo(e.pool), repository.NewBlockRepo(e.pool))
}

func TestBlock_Validation(t *testing.T) {
	e := newJoinEnv(t)
	svc := newBlockService(e)
	a := e.user(t, "a@example.com", "18_plus")
	b := e.user(t, "b@example.com", "18_plus")
	bare := e.bareUser(t, "bare@example.com")

	for name, target := range map[string]string{
		"self":          a,
		"self upper":    strings.ToUpper(a),
		"garbage":       "not-a-uuid",
		"empty":         "",
		"unknown":       "0190f0f0-0000-7000-8000-000000000001",
		"not onboarded": bare,
	} {
		if err := svc.Block(bg, a, target); err == nil {
			t.Fatalf("%s: no error", name)
		} else {
			wantFields(t, err, "user_id")
		}
	}
	if n := count(t, e.pool, `select count(*) from blocks`); n != 0 {
		t.Fatalf("blocks = %d", n)
	}

	if err := svc.Block(bg, a, strings.ToUpper(b)); err != nil {
		t.Fatal(err)
	}
	list, err := svc.List(bg, a)
	if err != nil || len(list) != 1 || list[0].UserID != b {
		t.Fatalf("list %+v err=%v", list, err)
	}
	if err := svc.Unblock(bg, a, strings.ToUpper(b)); err != nil {
		t.Fatal(err)
	}
	if list, _ := svc.List(bg, a); len(list) != 0 {
		t.Fatalf("after unblock %+v", list)
	}
	// Unblocking someone never blocked is fine; a malformed id is not.
	if err := svc.Unblock(bg, a, b); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unblock(bg, a, "nope"); err == nil {
		t.Fatal("garbage unblock accepted")
	} else {
		wantFields(t, err, "user_id")
	}

	// The caller must be onboarded for all three.
	if err := svc.Block(bg, bare, b); !errors.Is(err, ErrOnboardingRequired) {
		t.Fatalf("block err=%v", err)
	}
	if err := svc.Unblock(bg, bare, b); !errors.Is(err, ErrOnboardingRequired) {
		t.Fatalf("unblock err=%v", err)
	}
	if _, err := svc.List(bg, bare); !errors.Is(err, ErrOnboardingRequired) {
		t.Fatalf("list err=%v", err)
	}
}

func TestBlock_IdempotentAndLimit(t *testing.T) {
	e := newJoinEnv(t)
	svc := newBlockService(e)
	a := e.user(t, "a@example.com", "18_plus")
	b := e.user(t, "b@example.com", "18_plus")
	c := e.user(t, "c@example.com", "18_plus")

	if err := svc.Block(bg, a, b); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `with u as (insert into users(onboarded_at) select now() from generate_series(1, $2::int - 1) returning id)
		insert into blocks(blocker_id, blocked_id) select $1, id from u`, a, blockLimit)
	if n := count(t, e.pool, `select count(*) from blocks where blocker_id=$1`, a); n != blockLimit {
		t.Fatalf("seeded %d", n)
	}
	if err := svc.Block(bg, a, c); !errors.Is(err, ErrBlockLimit) {
		t.Fatalf("over limit err=%v", err)
	}
	if err := svc.Block(bg, a, b); err != nil {
		t.Fatalf("repeat block err=%v", err)
	}
	if n := count(t, e.pool, `select count(*) from blocks where blocker_id=$1`, a); n != blockLimit {
		t.Fatalf("blocks = %d", n)
	}
	// c's own list is unaffected by a's.
	if err := svc.Block(bg, c, a); err != nil {
		t.Fatal(err)
	}
}

// --- effects ---

func (e *chatEnv) blockWith(t *testing.T) *repository.BlockRepo {
	t.Helper()
	b := repository.NewBlockRepo(e.pool)
	e.svc.WithBlocks(b)
	return b
}

// newTo returns the message.new events addressed to user.
func (e *chatEnv) newTo(user string) []Event {
	var out []Event
	for _, ev := range e.pub.ofType(EventMessageNew) {
		if len(ev.To) == 1 && ev.To[0] == user {
			out = append(out, ev)
		}
	}
	return out
}

func TestSend_GroupSkipsBlockedEitherWay(t *testing.T) {
	e := newChatEnv(t, 2)
	blocks := e.blockWith(t)
	a := e.owner
	b := e.seat(t, "b@example.com", "zh-Hans", "18_plus", 2)
	c := e.seat(t, "c@example.com", "ko", "18_plus", 3)
	if err := blocks.Block(bg, a, b); err != nil {
		t.Fatal(err)
	}

	v, err := e.svc.Send(bg, b, e.room, text("b1", "你好"))
	if err != nil {
		t.Fatal(err)
	}
	rows := e.rows(t, v.ID)
	if _, ok := rows[a]; ok || len(rows) != 1 {
		t.Fatalf("B's message rows %v", rows)
	}
	if _, ok := rows[c]; !ok {
		t.Fatal("C has no delivery of B's message")
	}
	if len(e.newTo(a)) != 0 || len(e.newTo(c)) != 1 || len(e.newTo(b)) != 1 {
		t.Fatalf("events a=%d b=%d c=%d", len(e.newTo(a)), len(e.newTo(b)), len(e.newTo(c)))
	}

	e.pub.reset()
	v, err = e.svc.Send(bg, a, e.room, text("a1", "你好"))
	if err != nil {
		t.Fatal(err)
	}
	rows = e.rows(t, v.ID)
	if _, ok := rows[b]; ok || len(rows) != 1 {
		t.Fatalf("A's message rows %v", rows)
	}
	if len(e.newTo(b)) != 0 || len(e.newTo(c)) != 1 {
		t.Fatalf("events b=%d c=%d", len(e.newTo(b)), len(e.newTo(c)))
	}
}

func TestSend_DMToBlockedPeerSilent(t *testing.T) {
	e := newChatEnv(t, 2)
	blocks := e.blockWith(t)
	a := e.owner
	b := e.seat(t, "b@example.com", "ko", "18_plus", 2)
	if err := blocks.Block(bg, a, b); err != nil {
		t.Fatal(err)
	}
	in := text("d1", "你好")
	in.Scope, in.ToUserID = model.ScopeDM, a
	v, err := e.svc.Send(bg, b, e.room, in)
	if err != nil {
		t.Fatal(err)
	}
	if v.Scope != model.ScopeDM || v.ToUserID == nil || *v.ToUserID != a || v.ID == "" {
		t.Fatalf("view %+v", v)
	}
	if n := count(t, e.pool, `select count(*) from messages where id=$1`, v.ID); n != 1 {
		t.Fatalf("messages = %d", n)
	}
	if n := count(t, e.pool, `select count(*) from deliveries where message_id=$1`, v.ID); n != 0 {
		t.Fatalf("deliveries = %d", n)
	}
	if len(e.newTo(a)) != 0 || len(e.newTo(b)) != 1 || len(e.q.calls()) != 0 {
		t.Fatalf("events a=%d b=%d enqueued=%v", len(e.newTo(a)), len(e.newTo(b)), e.q.calls())
	}
}

func listIDs(t *testing.T, svc *ChatService, user, room string) map[string]bool {
	t.Helper()
	page, err := svc.List(bg, user, room, model.PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, v := range page.Items {
		out[v.ID] = true
	}
	return out
}

func TestList_HidesBlockedBothWaysAndRestoresOnUnblock(t *testing.T) {
	e := newChatEnv(t, 2)
	blocks := e.blockWith(t)
	svc := NewBlockService(repository.NewUserRepo(e.pool), blocks)
	a := e.owner
	b := e.seat(t, "b@example.com", "zh-Hans", "18_plus", 2)
	c := e.seat(t, "c@example.com", "zh-Hans", "18_plus", 3)

	m1, err := e.svc.Send(bg, b, e.room, text("b1", "来自B"))
	if err != nil {
		t.Fatal(err)
	}
	m2, err := e.svc.Send(bg, a, e.room, text("a1", "来自A"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Block(bg, a, b); err != nil {
		t.Fatal(err)
	}
	if got := listIDs(t, e.svc, a, e.room); got[m1.ID] || !got[m2.ID] {
		t.Fatalf("A sees %v", got)
	}
	if got := listIDs(t, e.svc, b, e.room); !got[m1.ID] || got[m2.ID] {
		t.Fatalf("B sees %v", got)
	}
	if got := listIDs(t, e.svc, c, e.room); !got[m1.ID] || !got[m2.ID] {
		t.Fatalf("C sees %v", got)
	}

	m3, err := e.svc.Send(bg, b, e.room, text("b2", "屏蔽期间"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Unblock(bg, a, b); err != nil {
		t.Fatal(err)
	}
	if got := listIDs(t, e.svc, a, e.room); !got[m1.ID] || !got[m2.ID] || got[m3.ID] {
		t.Fatalf("A after unblock sees %v", got)
	}
	if got := listIDs(t, e.svc, b, e.room); !got[m1.ID] || !got[m2.ID] || !got[m3.ID] {
		t.Fatalf("B after unblock sees %v", got)
	}
}

func TestTranslate_SkipsPublishToBlocked(t *testing.T) {
	e := newTranslateEnv(t)
	j, k := e.seat(t, "j@example.com", 2), e.seat(t, "k@example.com", 3)
	msg := e.message(t, map[string]string{j: "ja", k: "ko"})
	blocks := repository.NewBlockRepo(e.pool)
	e.svc.WithBlocks(blocks)
	if err := blocks.Block(bg, j, e.owner); err != nil { // the receiver blocks the sender
		t.Fatal(err)
	}
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	if st, base := e.status(t, msg, j); st != model.TranslationDone || base == nil {
		t.Fatalf("ja row %s %v", st, base)
	}
	if n := len(e.translatedTo(j)); n != 0 {
		t.Fatalf("blocked receiver got %d message.translated", n)
	}
	if n := len(e.translatedTo(k)); n != 1 {
		t.Fatalf("other receiver got %d message.translated", n)
	}
}

// Controller ruling R1: message.removed only reaches receivers with no block.
func TestTranslate_WithdrawSkipsRemovedToBlocked(t *testing.T) {
	e := newModTranslateEnv(t)
	j, k := e.seat(t, "j@example.com", 2), e.seat(t, "k@example.com", 3)
	blocks := repository.NewBlockRepo(e.pool)
	e.svc.WithBlocks(blocks)
	e.tr.risk = map[string]string{"ko": "block"}
	msg, _ := e.messageOrdered(t, []dspec{{receiver: j, lang: "ja"}, {receiver: k, lang: "ko"}})
	if err := blocks.Block(bg, j, e.owner); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	if n := len(e.removedTo(j)); n != 0 {
		t.Fatalf("blocked receiver got %d message.removed", n)
	}
	if n := len(e.removedTo(k)); n != 1 {
		t.Fatalf("other receiver got %d message.removed", n)
	}
	if n := len(e.pub.ofType(EventMessageTranslated)); n != 0 {
		t.Fatalf("message.translated = %d", n)
	}
}

// A wordlist-hidden group's message.removed also skips a blocked receiver.
func TestTranslate_HiddenGroupSkipsRemovedToBlocked(t *testing.T) {
	e := newModTranslateEnv(t)
	j := e.seat(t, "j@example.com", 2)
	blocks := repository.NewBlockRepo(e.pool)
	e.svc.WithBlocks(blocks)
	e.tr.text = func(string) string { return "that is bad" }
	msg, _ := e.messageOrdered(t, []dspec{{receiver: j, lang: "ja"}})
	if err := blocks.Block(bg, e.owner, j); err != nil { // the sender blocks the receiver
		t.Fatal(err)
	}
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	if n := len(e.pub.all()); n != 0 {
		t.Fatalf("events = %d", n)
	}
}

func TestPolishButton_BlockedPairNotFound(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	e.blockWith(t)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, polishBase, model.KindText)
	if err := repository.NewBlockRepo(e.pool).Block(bg, k, e.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Polish(bg, e.owner, did); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("err=%v", err)
	}
	if n := e.lim.n(e.owner); n != 0 {
		t.Fatalf("quota taken %d", n)
	}
	var st string
	if err := e.pool.QueryRow(bg, `select polish_status from deliveries where id=$1`, did).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != model.PolishNone || len(e.pq.list()) != 0 {
		t.Fatalf("polish_status=%s enqueued=%v", st, e.pq.list())
	}
}

func TestPolish_BlockedPairFallsBackWithoutModel(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	e.exec(t, `update users set mbti='ISTJ' where id=$1`, k)
	_, did := e.polishDelivery(t, k, "zh-Hans", "ko", polishBase, "내일 오후 세 시에 회의가 있으니 자료를 꼭 챙겨 오세요")
	blocks := repository.NewBlockRepo(e.pool)
	e.psvc.WithBlocks(blocks)
	if err := blocks.Block(bg, e.owner, k); err != nil {
		t.Fatal(err)
	}
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if st, txt := e.polishRow(t, did); st != model.PolishFallback || txt != nil {
		t.Fatalf("row %s %v", st, txt)
	}
	if e.po.count() != 0 || len(e.pub.all()) != 0 {
		t.Fatalf("model calls %d events %d", e.po.count(), len(e.pub.all()))
	}
}

func TestPolish_GiveUpSkipsPublishToBlocked(t *testing.T) {
	e := newPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	msg, did := e.polishDelivery(t, k, "zh-Hans", "ko", "明天见面聊", "x")
	e.exec(t, `update deliveries set translation_status='failed', base_text=null where id=$1`, did)
	e.exec(t, `update messages set created_at=$2 where id=$1`, msg, e.now.Add(-25*time.Hour))
	blocks := repository.NewBlockRepo(e.pool)
	e.psvc.WithBlocks(blocks)
	if err := blocks.Block(bg, k, e.owner); err != nil {
		t.Fatal(err)
	}
	if n, err := e.psvc.GiveUp(bg); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if st, _ := e.polishRow(t, did); st != model.PolishFallback {
		t.Fatalf("status %s", st)
	}
	if n := len(e.polishedTo(k)); n != 0 {
		t.Fatalf("message.polished = %d", n)
	}
}
