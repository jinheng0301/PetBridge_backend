package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/repository"
)

// chatQueue records every enqueued message id and fails when err is set.
type chatQueue struct {
	mu  sync.Mutex
	err error
	ids []string
}

func (q *chatQueue) EnqueueTranslate(_ context.Context, messageID, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ids = append(q.ids, messageID)
	return q.err
}

func (q *chatQueue) calls() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.ids...)
}

type chatEnv struct {
	*joinEnv
	repo   *repository.MessageRepo
	q      *chatQueue
	inline *InlinePool
	ran    chan string // message ids the inline pool ran
	block  chan struct{}
	svc    *ChatService
	owner  string
	room   string
}

// newChatEnv builds a ChatService over a zh-Hans owner's room. poolSize sizes the inline pool.
func newChatEnv(t *testing.T, poolSize int) *chatEnv {
	t.Helper()
	je := newJoinEnv(t)
	e := &chatEnv{joinEnv: je, repo: repository.NewMessageRepo(je.pool), q: &chatQueue{},
		ran: make(chan string, 10), block: make(chan struct{})}
	e.inline = NewInlinePool(poolSize, func(_ context.Context, id string) error {
		e.ran <- id
		<-e.block
		return nil
	}, discardLog())
	t.Cleanup(func() { close(e.block) })
	e.owner = e.langUser(t, "owner@example.com", "zh-Hans", "18_plus")
	e.room = je.roomOf(t, e.owner)
	e.svc = NewChatService(je.txm, repository.NewUserRepo(je.pool), repository.NewRoomMemberRepo(je.pool), e.repo,
		PassModerator{}, e.q, e.inline, je.pub, func() time.Time { return je.now }, discardLog())
	return e
}

func (e *chatEnv) langUser(t *testing.T, email, lang, ageBand string) string {
	t.Helper()
	id := e.bareUser(t, email)
	in := valid()
	in.NativeLang, in.AgeBand = lang, ageBand
	if _, err := e.users.Onboard(bg, id, in); err != nil {
		t.Fatal(err)
	}
	return id
}

// seat creates a user with lang and age band and seats them in the owner's room.
func (e *chatEnv) seat(t *testing.T, email, lang, ageBand string, seatNo int) string {
	t.Helper()
	id := e.langUser(t, email, lang, ageBand)
	e.exec(t, `insert into room_members(room_id, user_id, seat_no, role) values($1,$2,$3,'member')`, e.room, id, seatNo)
	return id
}

func (e *chatEnv) rows(t *testing.T, msgID string) map[string]struct {
	status string
	base   *string
} {
	t.Helper()
	rs, err := e.pool.Query(bg, `select receiver_id::text, translation_status, base_text from deliveries where message_id=$1`, msgID)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]struct {
		status string
		base   *string
	}{}
	for rs.Next() {
		var id, st string
		var base *string
		if err := rs.Scan(&id, &st, &base); err != nil {
			t.Fatal(err)
		}
		out[id] = struct {
			status string
			base   *string
		}{st, base}
	}
	return out
}

func text(cmid, s string) SendInput {
	return SendInput{ClientMsgID: cmid, Kind: model.KindText, Text: s}
}

func wantFields(t *testing.T, err error, field string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Fields) != 1 || ve.Fields[0] != field {
		t.Fatalf("err = %v, want ValidationError{%s}", err, field)
	}
}

func TestSend_GroupCreatesDeliveries(t *testing.T) {
	e := newChatEnv(t, 2)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	j := e.seat(t, "j@example.com", "ja", "18_plus", 3)
	z := e.seat(t, "z@example.com", "zh-Hans", "18_plus", 4)

	v, err := e.svc.Send(bg, e.owner, e.room, text("c1", "  你好  "))
	if err != nil {
		t.Fatal(err)
	}
	if v.Text != "你好" || v.SrcLang != "zh-Hans" || v.Delivery != nil || v.ClientMsgID == nil || *v.ClientMsgID != "c1" ||
		v.Scope != model.ScopeRoom || v.Kind != model.KindText || v.PolishRequested {
		t.Fatalf("view %+v", v)
	}
	rows := e.rows(t, v.ID)
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	if r := rows[z]; r.status != model.TranslationDone || r.base == nil || *r.base != "你好" {
		t.Fatalf("zh row %+v", r)
	}
	for _, u := range []string{k, j} {
		if r := rows[u]; r.status != model.TranslationPending || r.base != nil {
			t.Fatalf("row %+v", r)
		}
	}
	evs := e.pub.ofType(EventMessageNew)
	if len(evs) != 4 {
		t.Fatalf("message.new = %d", len(evs))
	}
	to := map[string]bool{}
	for _, ev := range evs {
		if ev.Kind != KindTransient || ev.RoomID != e.room || len(ev.To) != 1 {
			t.Fatalf("event %+v", ev)
		}
		to[ev.To[0]] = true
		m := ev.Data["message"].(model.MessageView)
		if (ev.To[0] == e.owner) != (m.Delivery == nil) {
			t.Fatalf("view for %s: %+v", ev.To[0], m)
		}
	}
	for _, u := range []string{e.owner, k, j, z} {
		if !to[u] {
			t.Fatalf("no event to %s", u)
		}
	}
	if c := e.q.calls(); len(c) != 1 || c[0] != v.ID {
		t.Fatalf("queue %v", c)
	}
}

func TestSend_PhraseNeedsNoAI(t *testing.T) {
	e := newChatEnv(t, 2)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	j := e.seat(t, "j@example.com", "ja", "18_plus", 3)

	v, err := e.svc.Send(bg, e.owner, e.room, SendInput{ClientMsgID: "p1", Kind: model.KindPhrase, PhraseID: "good_morning"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Text != "早上好" || v.PhraseID == nil || *v.PhraseID != "good_morning" {
		t.Fatalf("view %+v", v)
	}
	rows := e.rows(t, v.ID)
	for u, want := range map[string]string{k: "좋은 아침이에요", j: "おはようございます"} {
		if r := rows[u]; r.status != model.TranslationDone || r.base == nil || *r.base != want {
			t.Fatalf("row %+v", r)
		}
	}
	if c := e.q.calls(); len(c) != 0 {
		t.Fatalf("queue %v", c)
	}

	_, err = e.svc.Send(bg, e.owner, e.room, SendInput{ClientMsgID: "p2", Kind: model.KindPhrase, PhraseID: "nope"})
	wantFields(t, err, "phrase_id")
	_, err = e.svc.Send(bg, e.owner, e.room, SendInput{ClientMsgID: "p3", Kind: "sticker"})
	wantFields(t, err, "kind")
	_, err = e.svc.Send(bg, e.owner, e.room, text("a b", "hi"))
	wantFields(t, err, "client_msg_id")
	_, err = e.svc.Send(bg, e.owner, e.room, text(strings.Repeat("a", 65), "hi"))
	wantFields(t, err, "client_msg_id")
	_, err = e.svc.Send(bg, e.owner, e.room, text("", "hi"))
	wantFields(t, err, "client_msg_id")
	_, err = e.svc.Send(bg, e.owner, e.room, SendInput{ClientMsgID: "s1", Kind: model.KindText, Text: "hi", Scope: "group"})
	wantFields(t, err, "scope")
	if _, err := e.svc.Send(bg, e.owner, e.room, text(strings.Repeat("a", 64), "hi")); err != nil {
		t.Fatalf("64-char client_msg_id: %v", err)
	}
}

func TestSend_DM(t *testing.T) {
	e := newChatEnv(t, 2)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	j := e.seat(t, "j@example.com", "ja", "18_plus", 3)
	teen := e.seat(t, "teen@example.com", "ja", "13_17", 4)
	outsider := e.langUser(t, "out@example.com", "ko", "18_plus")

	dm := func(cmid, to string) SendInput {
		return SendInput{ClientMsgID: cmid, Kind: model.KindText, Text: "hi", Scope: model.ScopeDM, ToUserID: to}
	}
	v, err := e.svc.Send(bg, e.owner, e.room, dm("d1", k))
	if err != nil {
		t.Fatal(err)
	}
	if v.Scope != model.ScopeDM || v.ToUserID == nil || *v.ToUserID != k {
		t.Fatalf("view %+v", v)
	}
	rows := e.rows(t, v.ID)
	if _, ok := rows[k]; len(rows) != 1 || !ok {
		t.Fatalf("rows %+v", rows)
	}
	if n := len(e.pub.ofType(EventMessageNew)); n != 2 {
		t.Fatalf("message.new = %d", n)
	}
	_ = j

	for name, to := range map[string]string{"self": e.owner, "outsider": outsider, "missing": "", "bad": "not-a-uuid"} {
		_, err := e.svc.Send(bg, e.owner, e.room, dm("d-"+name, to))
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Fields[0] != "to_user_id" {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if _, err := e.svc.Send(bg, teen, e.room, dm("d-teen", k)); !errors.Is(err, ErrAgeRestricted) {
		t.Fatalf("teen sender: %v", err)
	}
	if _, err := e.svc.Send(bg, e.owner, e.room, dm("d-to-teen", teen)); !errors.Is(err, ErrAgeRestricted) {
		t.Fatalf("teen receiver: %v", err)
	}
}

func TestSend_TextBoundaries(t *testing.T) {
	e := newChatEnv(t, 2)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)

	_, err := e.svc.Send(bg, e.owner, e.room, text("t0", "   "))
	wantFields(t, err, "text")
	for i, s := range []string{"👨‍👩‍👧", "！！？"} {
		v, err := e.svc.Send(bg, e.owner, e.room, text("t"+string(rune('1'+i)), s))
		if err != nil {
			t.Fatal(err)
		}
		if r := e.rows(t, v.ID)[k]; r.status != model.TranslationDone || r.base == nil || *r.base != s {
			t.Fatalf("%q: row %+v", s, r)
		}
	}
	if c := e.q.calls(); len(c) != 0 {
		t.Fatalf("queue %v", c)
	}
	if _, err := e.svc.Send(bg, e.owner, e.room, text("t500", strings.Repeat("あ", 500))); err != nil {
		t.Fatalf("500 runes: %v", err)
	}
	_, err = e.svc.Send(bg, e.owner, e.room, text("t501", strings.Repeat("あ", 501)))
	wantFields(t, err, "text")
}

func TestNothingToTranslate(t *testing.T) {
	for s, want := range map[string]bool{"": false, "！！": true, "👍👍": true, "👨‍👩‍👧": true, " ?! ": true,
		"ok!": false, "你好": false, "1": false} {
		if got := nothingToTranslate(s); got != want {
			t.Errorf("%q = %v", s, got)
		}
	}
}

func TestSend_NotSeated(t *testing.T) {
	e := newChatEnv(t, 2)
	outsider := e.langUser(t, "out@example.com", "ko", "18_plus")
	if _, err := e.svc.Send(bg, outsider, e.room, text("n1", "hi")); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	if _, err := e.svc.Send(bg, e.owner, "not-a-uuid", text("n2", "hi")); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("bad room: %v", err)
	}
}

func TestSend_ConcurrentSameClientMsgID(t *testing.T) {
	e := newChatEnv(t, 2)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	_ = k

	start := make(chan struct{})
	var wg sync.WaitGroup
	ids := make([]string, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := e.svc.Send(bg, e.owner, e.room, text("same", "你好"))
			ids[i], errs[i] = v.ID, err
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("ids %v", ids)
	}
	if n := count(t, e.pool, `select count(*) from messages`); n != 1 {
		t.Fatalf("messages = %d", n)
	}
	if n := len(e.pub.ofType(EventMessageNew)); n != 2 {
		t.Fatalf("message.new = %d", n)
	}
	if c := e.q.calls(); len(c) != 1 {
		t.Fatalf("queue %v", c)
	}

	// A later replay, even with another body or in another room, returns the original without events.
	other := e.roomOf(t, k)
	e.exec(t, `insert into room_members(room_id, user_id, seat_no, role) values($1,$2,2,'member')`, other, e.owner)
	e.pub.reset()
	v, err := e.svc.Send(bg, e.owner, other, text("same", "different"))
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != ids[0] || v.Text != "你好" || v.RoomID != e.room {
		t.Fatalf("replay %+v", v)
	}
	if len(e.pub.all()) != 0 || len(e.q.calls()) != 1 {
		t.Fatalf("replay published or enqueued")
	}
}

func TestSend_EnqueueFailsRunsInline(t *testing.T) {
	e := newChatEnv(t, 2)
	e.seat(t, "k@example.com", "ko", "18_plus", 2)
	e.q.err = errors.New("redis down")

	v, err := e.svc.Send(bg, e.owner, e.room, text("e1", "你好"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-e.ran:
		if id != v.ID {
			t.Fatalf("inline ran %s, want %s", id, v.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inline pool did not run")
	}
	select {
	case id := <-e.ran:
		t.Fatalf("inline ran twice (%s)", id)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSend_InlinePoolFullLeavesPending(t *testing.T) {
	e := newChatEnv(t, 1)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	e.q.err = errors.New("redis down")
	if !e.inline.TryRun("occupier") {
		t.Fatal("could not fill pool")
	}
	<-e.ran

	v, err := e.svc.Send(bg, e.owner, e.room, text("f1", "你好"))
	if err != nil {
		t.Fatal(err)
	}
	if r := e.rows(t, v.ID)[k]; r.status != model.TranslationPending {
		t.Fatalf("row %+v", r)
	}
	select {
	case id := <-e.ran:
		t.Fatalf("inline ran %s on a full pool", id)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestList_Visibility(t *testing.T) {
	e := newChatEnv(t, 2)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	j := e.seat(t, "j@example.com", "ja", "18_plus", 3)

	room, err := e.svc.Send(bg, e.owner, e.room, text("l1", "大家好"))
	if err != nil {
		t.Fatal(err)
	}
	dm, err := e.svc.Send(bg, e.owner, e.room, SendInput{ClientMsgID: "l2", Kind: model.KindText, Text: "悄悄话",
		Scope: model.ScopeDM, ToUserID: k})
	if err != nil {
		t.Fatal(err)
	}
	late := e.seat(t, "late@example.com", "ko", "18_plus", 4)

	ids := func(user string) []string {
		t.Helper()
		p, err := e.svc.List(bg, user, e.room, model.PageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Items == nil {
			t.Fatalf("%s: items is nil", user)
		}
		var out []string
		for _, v := range p.Items {
			out = append(out, v.ID)
		}
		return out
	}
	eq := func(got, want []string) bool { return strings.Join(got, ",") == strings.Join(want, ",") }
	if got := ids(e.owner); !eq(got, []string{room.ID, dm.ID}) {
		t.Fatalf("owner %v", got)
	}
	if got := ids(k); !eq(got, []string{room.ID, dm.ID}) {
		t.Fatalf("k %v", got)
	}
	if got := ids(j); !eq(got, []string{room.ID}) {
		t.Fatalf("third party %v", got)
	}
	if got := ids(late); len(got) != 0 {
		t.Fatalf("late joiner %v", got)
	}

	p, err := e.svc.List(bg, e.owner, e.room, model.PageQuery{After: room.ID})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != dm.ID || p.HasMore {
		t.Fatalf("after: %+v %v", p, err)
	}
	p, err = e.svc.List(bg, e.owner, e.room, model.PageQuery{Limit: 1})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != dm.ID || !p.HasMore {
		t.Fatalf("limit 1: %+v %v", p, err)
	}
}

func TestList_LeftMemberNotFound(t *testing.T) {
	e := newChatEnv(t, 2)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	if _, err := e.svc.Send(bg, e.owner, e.room, text("x1", "你好")); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `update room_members set left_at=now(), left_reason='left' where room_id=$1 and user_id=$2`, e.room, k)
	if _, err := e.svc.List(bg, k, e.room, model.PageQuery{}); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("left member: %v", err)
	}
	if _, err := e.svc.List(bg, e.owner, "not-a-uuid", model.PageQuery{}); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("bad room: %v", err)
	}
}

func TestList_BadQuery(t *testing.T) {
	e := newChatEnv(t, 2)
	id := "01890a5d-ac96-774b-bcce-b302099a8057"
	cases := []struct {
		q     model.PageQuery
		field string
	}{
		{model.PageQuery{Before: id, After: id}, "before"},
		{model.PageQuery{Limit: 101}, "limit"},
		{model.PageQuery{Limit: -1}, "limit"},
		{model.PageQuery{Before: "nope"}, "before"},
		{model.PageQuery{After: "nope"}, "after"},
	}
	for _, c := range cases {
		_, err := e.svc.List(bg, e.owner, e.room, c.q)
		wantFields(t, err, c.field)
	}
	p, err := e.svc.List(bg, e.owner, e.room, model.PageQuery{Limit: 100})
	if err != nil || p.Items == nil || len(p.Items) != 0 {
		t.Fatalf("empty page: %+v %v", p, err)
	}
}
