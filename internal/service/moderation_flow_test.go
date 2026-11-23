package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/repository"
)

// flagRow is one mod_flags row as the tests read it.
type flagRow struct {
	stage, verdict, source, userID            string
	terms                                     []string
	roomID, messageID, deliveryID, lang, text *string
}

func modFlags(t *testing.T, p *pgxpool.Pool) []flagRow {
	t.Helper()
	rs, err := p.Query(bg, `select stage, verdict, source, terms, user_id::text, room_id::text, message_id::text,
		delivery_id::text, target_lang, text from mod_flags order by created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []flagRow
	for rs.Next() {
		var f flagRow
		if err := rs.Scan(&f.stage, &f.verdict, &f.source, &f.terms, &f.userID, &f.roomID, &f.messageID,
			&f.deliveryID, &f.lang, &f.text); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func is(p *string, want string) bool { return p != nil && *p == want }

// blockAllModerator blocks every text.
type blockAllModerator struct{}

func (blockAllModerator) CheckOutgoing(context.Context, string, string) (ModResult, error) {
	return ModResult{Verdict: ModBlock, Source: ModSourceWordlist}, nil
}

func (blockAllModerator) CheckTranslated(context.Context, string, string, string) (ModResult, error) {
	return ModResult{Verdict: ModBlock, Source: ModSourceWordlist}, nil
}

// --- send ---

func newModChatEnv(t *testing.T) *polishChatEnv {
	t.Helper()
	e := newPolishChatEnv(t, 30, true)
	e.svc.mod = testModerator()
	e.svc.WithModFlags(repository.NewModFlagRepo(e.pool))
	return e
}

func TestSend_BlockedWordRefused(t *testing.T) {
	e := newModChatEnv(t)
	e.seat(t, "k@example.com", "ko", "18_plus", 2)
	const body = "this is a bad idea for the meeting"
	_, err := e.svc.Send(bg, e.owner, e.room, polishText("c1", "  "+body+" "))
	var cb *ContentBlockedError
	if !errors.As(err, &cb) || len(cb.Fields) != 0 || err.Error() != "content blocked" {
		t.Fatalf("err=%v", err)
	}
	if n := count(t, e.pool, `select count(*) from messages`); n != 0 {
		t.Fatalf("messages = %d", n)
	}
	if n := e.lim.n(e.owner); n != 0 {
		t.Fatalf("polish quota taken %d", n)
	}
	if len(e.pub.all()) != 0 || len(e.q.calls()) != 0 || len(e.pq.list()) != 0 {
		t.Fatal("events or tasks for a blocked message")
	}
	fs := modFlags(t, e.pool)
	if len(fs) != 1 {
		t.Fatalf("flags %+v", fs)
	}
	f := fs[0]
	if f.stage != model.StageOutgoing || f.verdict != model.ModBlock || f.source != ModSourceWordlist ||
		f.userID != e.owner || !is(f.roomID, e.room) || !is(f.text, body) || f.messageID != nil ||
		f.deliveryID != nil || len(f.terms) != 1 || f.terms[0] != "bad" {
		t.Fatalf("flag %+v", f)
	}
}

func TestSend_BlockedReplayStillBlocked(t *testing.T) {
	e := newModChatEnv(t)
	e.seat(t, "k@example.com", "ko", "18_plus", 2)
	for i := 0; i < 2; i++ {
		var cb *ContentBlockedError
		if _, err := e.svc.Send(bg, e.owner, e.room, text("c1", "so bad")); !errors.As(err, &cb) {
			t.Fatalf("try %d err=%v", i, err)
		}
	}
	if n := len(modFlags(t, e.pool)); n != 2 {
		t.Fatalf("flags = %d", n)
	}
}

func TestSend_ReviewWordStoredAndFlagged(t *testing.T) {
	e := newModChatEnv(t)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	v, err := e.svc.Send(bg, e.owner, e.room, text("c1", "meh, fine"))
	if err != nil {
		t.Fatal(err)
	}
	var st string
	if err := e.pool.QueryRow(bg, `select mod_status from messages where id=$1`, v.ID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != model.ModReview {
		t.Fatalf("mod_status=%s", st)
	}
	got := false
	for _, ev := range e.pub.ofType(EventMessageNew) {
		if len(ev.To) == 1 && ev.To[0] == k {
			got = true
		}
	}
	if !got {
		t.Fatal("receiver got no message.new")
	}
	fs := modFlags(t, e.pool)
	if len(fs) != 1 {
		t.Fatalf("flags %+v", fs)
	}
	f := fs[0]
	if f.stage != model.StageOutgoing || f.verdict != model.ModReview || !is(f.messageID, v.ID) || f.text != nil ||
		len(f.terms) != 1 || f.terms[0] != "meh" {
		t.Fatalf("flag %+v", f)
	}
}

func TestSend_PhraseSkipsModeration(t *testing.T) {
	e := newModChatEnv(t)
	e.seat(t, "k@example.com", "ko", "18_plus", 2)
	e.svc.mod = blockAllModerator{}
	if _, err := e.svc.Send(bg, e.owner, e.room, SendInput{ClientMsgID: "c1", Kind: model.KindPhrase, PhraseID: "thank_you"}); err != nil {
		t.Fatal(err)
	}
	var cb *ContentBlockedError
	if _, err := e.svc.Send(bg, e.owner, e.room, text("c2", "hello")); !errors.As(err, &cb) {
		t.Fatalf("text err=%v", err)
	}
}

// --- translation ---

type modTranslateEnv struct {
	*translateEnv
	pq  *recPolishQueue
	seq int
}

func newModTranslateEnv(t *testing.T) *modTranslateEnv {
	t.Helper()
	e := &modTranslateEnv{translateEnv: newTranslateEnv(t), pq: &recPolishQueue{}}
	e.svc.mod = testModerator()
	e.svc.WithModFlags(repository.NewModFlagRepo(e.pool)).WithPolishQueue(e.pq)
	return e
}

// dspec is one delivery of a test message; done rows get the message body as base.
type dspec struct {
	receiver, lang string
	done           bool
	polish         string
}

// message stores a zh text message from the owner whose deliveries are
// processed in the order given (their ids ascend).
func (e *modTranslateEnv) messageOrdered(t *testing.T, ds []dspec) (string, map[string]string) {
	t.Helper()
	e.seq++
	var id string
	if err := e.pool.QueryRow(bg, `select gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	m := model.NewMessage{ID: id, RoomID: e.room, SenderID: e.owner, Scope: model.ScopeRoom, Kind: model.KindText,
		SrcLang: "zh-Hans", Body: "你好", ClientMsgID: "c-" + id}
	if ok, err := e.repo.InsertMessage(bg, m); err != nil || !ok {
		t.Fatal(ok, err)
	}
	ids := map[string]string{}
	for i, d := range ds {
		did := fmt.Sprintf("%08x-0000-4000-8000-%012x", e.seq, i+1)
		st, polish := model.TranslationPending, d.polish
		var base, by *string
		if d.done {
			st, base = model.TranslationDone, &m.Body
		}
		if polish == "" {
			polish = model.PolishNone
		}
		if polish == model.PolishPending {
			by = &e.owner
		}
		e.exec(t, `insert into deliveries(id, message_id, receiver_id, target_lang, base_text, translation_status, polish_status, polish_by)
			values($1,$2,$3,$4,$5,$6,$7,$8)`, did, id, d.receiver, d.lang, base, st, polish, by)
		ids[d.receiver] = did
	}
	return id, ids
}

func (e *modTranslateEnv) row(t *testing.T, deliveryID string) (trans, polish, mod string, base *string) {
	t.Helper()
	if err := e.pool.QueryRow(bg, `select translation_status, polish_status, mod_status, base_text from deliveries where id=$1`,
		deliveryID).Scan(&trans, &polish, &mod, &base); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *modTranslateEnv) removedTo(user string) []Event {
	var out []Event
	for _, ev := range e.pub.ofType(EventMessageRemoved) {
		if len(ev.To) == 1 && ev.To[0] == user {
			out = append(out, ev)
		}
	}
	return out
}

func TestTranslate_WordlistBlockHidesGroupOnly(t *testing.T) {
	e := newModTranslateEnv(t)
	j, k := e.seat(t, "j@example.com", 2), e.seat(t, "k@example.com", 3)
	e.tr.text = func(lang string) string {
		if lang == "ja" {
			return "that is bad"
		}
		return lang + ": hello"
	}
	msg, ids := e.messageOrdered(t, []dspec{{receiver: j, lang: "ja", polish: model.PolishPending}, {receiver: k, lang: "ko"}})

	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	trans, polish, mod, base := e.row(t, ids[j])
	if trans != model.TranslationDone || polish != model.PolishFallback || mod != model.ModBlock || !is(base, "that is bad") {
		t.Fatalf("ja row %s %s %s %v", trans, polish, mod, base)
	}
	if len(e.translatedTo(j)) != 0 || len(e.pub.ofType(EventMessagePolished)) != 0 {
		t.Fatal("ja got message.translated or polished")
	}
	evs := e.removedTo(j)
	if len(evs) != 1 || evs[0].Kind != KindTransient || evs[0].RoomID != e.room ||
		evs[0].Data["room_id"] != e.room || evs[0].Data["message_id"] != msg || len(evs[0].Data) != 2 {
		t.Fatalf("ja removed %+v", evs)
	}
	if len(e.pq.list()) != 0 {
		t.Fatalf("polish enqueued %v", e.pq.list())
	}
	if trans, _, mod, _ := e.row(t, ids[k]); trans != model.TranslationDone || mod != model.ModOK {
		t.Fatalf("ko row %s %s", trans, mod)
	}
	if len(e.translatedTo(k)) != 1 || len(e.removedTo(k)) != 0 {
		t.Fatal("ko events wrong")
	}
	var mst string
	e.pool.QueryRow(bg, `select mod_status from messages where id=$1`, msg).Scan(&mst)
	if mst != model.ModOK {
		t.Fatalf("message mod_status=%s", mst)
	}
	fs := modFlags(t, e.pool)
	if len(fs) != 1 {
		t.Fatalf("flags %+v", fs)
	}
	f := fs[0]
	if f.stage != model.StageTranslation || f.verdict != model.ModBlock || f.source != ModSourceWordlist ||
		f.userID != e.owner || !is(f.messageID, msg) || !is(f.deliveryID, ids[j]) || !is(f.lang, "ja") || f.text != nil {
		t.Fatalf("flag %+v", f)
	}
}

func TestTranslate_ModelBlockWithdrawsWholeMessage(t *testing.T) {
	e := newModTranslateEnv(t)
	j, k, z := e.seat(t, "j@example.com", 2), e.seat(t, "k@example.com", 3), e.seat(t, "z@example.com", 4)
	e.tr.risk = map[string]string{"ko": "block"}
	msg, ids := e.messageOrdered(t, []dspec{{receiver: j, lang: "ja", polish: model.PolishPending},
		{receiver: k, lang: "ko"}, {receiver: z, lang: "zh-Hans", done: true}})

	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{j, k, z} {
		if n := len(e.removedTo(u)); n != 1 {
			t.Fatalf("%s removed events = %d", u, n)
		}
		if _, _, mod, _ := e.row(t, ids[u]); mod != model.ModBlock {
			t.Fatalf("%s mod=%s", u, mod)
		}
	}
	if n := len(e.pub.ofType(EventMessageTranslated)); n != 0 {
		t.Fatalf("message.translated events = %d", n)
	}
	if len(e.pq.list()) != 0 {
		t.Fatalf("polish enqueued %v", e.pq.list())
	}
	var mst string
	e.pool.QueryRow(bg, `select mod_status from messages where id=$1`, msg).Scan(&mst)
	if mst != model.ModBlock {
		t.Fatalf("message mod_status=%s", mst)
	}
	fs := modFlags(t, e.pool)
	if len(fs) != 1 {
		t.Fatalf("flags %+v", fs)
	}
	if f := fs[0]; f.stage != model.StageTranslation || f.verdict != model.ModBlock || f.source != ModSourceModel ||
		!is(f.messageID, msg) || f.deliveryID != nil || f.text != nil {
		t.Fatalf("flag %+v", f)
	}

	calls := e.tr.callCount()
	e.pub.reset()
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	if e.tr.callCount() != calls || len(e.pub.all()) != 0 {
		t.Fatalf("second run: calls %d -> %d, events %d", calls, e.tr.callCount(), len(e.pub.all()))
	}
}

// A wordlist block with only a model review hides the group, not the message.
func TestTranslate_WordlistBlockModelReviewHidesGroupOnly(t *testing.T) {
	e := newModTranslateEnv(t)
	j, k := e.seat(t, "j@example.com", 2), e.seat(t, "k@example.com", 3)
	e.tr.risk = map[string]string{"ja": "review"}
	e.tr.text = func(lang string) string {
		if lang == "ja" {
			return "bad"
		}
		return "fine"
	}
	msg, ids := e.messageOrdered(t, []dspec{{receiver: j, lang: "ja"}, {receiver: k, lang: "ko"}})
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	if _, _, mod, _ := e.row(t, ids[j]); mod != model.ModBlock {
		t.Fatalf("ja mod=%s", mod)
	}
	if _, _, mod, _ := e.row(t, ids[k]); mod != model.ModOK || len(e.translatedTo(k)) != 1 {
		t.Fatalf("ko mod=%s", mod)
	}
	if fs := modFlags(t, e.pool); len(fs) != 1 || fs[0].source != ModSourceBoth || fs[0].deliveryID == nil {
		t.Fatalf("flags %+v", fs)
	}
}

func TestTranslate_ReviewFlagsAndShows(t *testing.T) {
	e := newModTranslateEnv(t)
	j, k := e.seat(t, "j@example.com", 2), e.seat(t, "k@example.com", 3)
	e.tr.risk = map[string]string{"ko": "review"}
	e.tr.text = func(lang string) string {
		if lang == "ja" {
			return "meh"
		}
		return "fine"
	}
	msg, ids := e.messageOrdered(t, []dspec{{receiver: j, lang: "ja", polish: model.PolishPending}, {receiver: k, lang: "ko"}})
	if err := e.svc.Process(bg, msg); err != nil {
		t.Fatal(err)
	}
	for u, want := range map[string]string{j: "meh", k: "fine"} {
		trans, _, mod, base := e.row(t, ids[u])
		if trans != model.TranslationDone || mod != model.ModReview || !is(base, want) {
			t.Fatalf("%s row %s %s %v", u, trans, mod, base)
		}
		if len(e.translatedTo(u)) != 1 || len(e.removedTo(u)) != 0 {
			t.Fatalf("%s events wrong", u)
		}
	}
	if got := e.pq.list(); len(got) != 1 || got[0] != ids[j] {
		t.Fatalf("polish enqueued %v", got)
	}
	fs := modFlags(t, e.pool)
	if len(fs) != 2 {
		t.Fatalf("flags %+v", fs)
	}
	src := map[string]string{}
	for _, f := range fs {
		if f.stage != model.StageTranslation || f.verdict != model.ModReview || f.deliveryID == nil || f.text != nil {
			t.Fatalf("flag %+v", f)
		}
		src[*f.lang] = f.source
	}
	if src["ja"] != ModSourceWordlist || src["ko"] != ModSourceModel {
		t.Fatalf("sources %v", src)
	}
}

// --- polish ---

func newModPolishEnv(t *testing.T) *polishEnv {
	t.Helper()
	e := newPolishEnv(t)
	e.psvc.mod = testModerator()
	e.psvc.WithModFlags(repository.NewModFlagRepo(e.pool))
	return e
}

const polishBase = "明天下午三点开会，记得带资料"

func TestPolish_BlockFallsBackAndFlags(t *testing.T) {
	e := newModPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	e.po.outcome = &AIOutcome{Text: "a bad rewrite of it", Risk: "none", Calls: 1}
	msg, did := e.polishDelivery(t, k, "zh-Hans", "ja", polishBase, polishBase)
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if st, txt := e.polishRow(t, did); st != model.PolishFallback || txt != nil {
		t.Fatalf("row %s %v", st, txt)
	}
	evs := e.polishedTo(k)
	if len(evs) != 1 || evs[0].Delivery.PolishStatus != model.PolishFallback {
		t.Fatalf("events %+v", evs)
	}
	fs := modFlags(t, e.pool)
	if len(fs) != 1 {
		t.Fatalf("flags %+v", fs)
	}
	if f := fs[0]; f.stage != model.StagePolish || f.verdict != model.ModBlock || !is(f.text, "a bad rewrite of it") ||
		!is(f.deliveryID, did) || !is(f.messageID, msg) || !is(f.lang, "ja") || f.userID != e.owner {
		t.Fatalf("flag %+v", f)
	}
}

func TestPolish_ReviewDoneAndFlags(t *testing.T) {
	e := newModPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	e.po.outcome = &AIOutcome{Text: "a meh rewrite of it", Risk: "none", Calls: 1}
	_, did := e.polishDelivery(t, k, "zh-Hans", "ja", polishBase, polishBase)
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if st, txt := e.polishRow(t, did); st != model.PolishDone || !is(txt, "a meh rewrite of it") {
		t.Fatalf("row %s %v", st, txt)
	}
	var mod string
	e.pool.QueryRow(bg, `select mod_status from deliveries where id=$1`, did).Scan(&mod)
	if mod != model.ModOK {
		t.Fatalf("delivery mod_status=%s", mod)
	}
	fs := modFlags(t, e.pool)
	if len(fs) != 1 || fs[0].stage != model.StagePolish || fs[0].verdict != model.ModReview || !is(fs[0].text, "a meh rewrite of it") {
		t.Fatalf("flags %+v", fs)
	}
}

func TestPolish_HiddenDeliverySkipsModel(t *testing.T) {
	e := newModPolishEnv(t)
	k := e.seat(t, "k@example.com", 2)
	_, did := e.polishDelivery(t, k, "zh-Hans", "ja", polishBase, polishBase)
	e.exec(t, `update deliveries set mod_status='block' where id=$1`, did)
	if err := e.psvc.Process(bg, did); err != nil {
		t.Fatal(err)
	}
	if st, txt := e.polishRow(t, did); st != model.PolishFallback || txt != nil {
		t.Fatalf("row %s %v", st, txt)
	}
	if e.po.count() != 0 || len(e.pub.all()) != 0 {
		t.Fatalf("calls=%d events=%d", e.po.count(), len(e.pub.all()))
	}
}

func TestPolishButton_HiddenDeliveryNotFoundNoCharge(t *testing.T) {
	e := newPolishChatEnv(t, 30, true)
	k := e.seat(t, "k@example.com", "ko", "18_plus", 2)
	did := e.translatedDelivery(t, k, e.owner, polishBase, model.KindText)
	e.exec(t, `update deliveries set mod_status='block' where id=$1`, did)
	if _, err := e.svc.Polish(bg, e.owner, did); !errors.Is(err, ErrDeliveryNotFound) {
		t.Fatalf("err=%v", err)
	}
	if e.lim.n(e.owner) != 0 || len(e.pq.list()) != 0 {
		t.Fatalf("taken=%d enqueued=%v", e.lim.n(e.owner), e.pq.list())
	}
	var st string
	e.pool.QueryRow(bg, `select polish_status from deliveries where id=$1`, did).Scan(&st)
	if st != model.PolishNone {
		t.Fatalf("polish_status=%s", st)
	}
}

// --- events ---

func TestMessageRemovedEvent(t *testing.T) {
	ev := messageRemovedEvent("r1", "m1", "u1")
	if ev.Type != model.FrameMessageRemoved || ev.Kind != KindTransient ||
		ev.RoomID != "r1" || len(ev.To) != 1 || ev.To[0] != "u1" ||
		ev.Data["room_id"] != "r1" || ev.Data["message_id"] != "m1" || len(ev.Data) != 2 {
		t.Fatalf("%+v", ev)
	}
}

// No log line carries message text, translations, polish output or matched terms.
func TestModeration_LogsCarryNoText(t *testing.T) {
	var logs strings.Builder
	e := newModChatEnv(t)
	e.svc.log = slog.New(slog.NewTextHandler(&logs, nil))
	e.seat(t, "k@example.com", "ko", "18_plus", 2)
	e.svc.Send(bg, e.owner, e.room, text("c1", "zzsecret bad"))
	e.svc.Send(bg, e.owner, e.room, text("c2", "zzsecret meh"))
	if out := logs.String(); strings.Contains(out, "zzsecret") || strings.Contains(out, "bad") || strings.Contains(out, "meh") {
		t.Fatalf("logs %q", out)
	}
}
