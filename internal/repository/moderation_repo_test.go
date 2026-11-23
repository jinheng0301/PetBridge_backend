package repository_test

import (
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/service"
)

var (
	_ service.ModFlagStore = (*repository.ModFlagRepo)(nil)
	_ service.BlockStore   = (*repository.BlockRepo)(nil)
	_ service.ReportStore  = (*repository.ReportRepo)(nil)
)

func TestBlockRepo(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewBlockRepo(p)
	a, b, c := mkUser(t, p), mkUser(t, p), mkUser(t, p)
	if _, err := p.Exec(bg, `update users set nickname='Bee', pet_type='cat' where id=$1`, b); err != nil {
		t.Fatal(err)
	}
	if err := repo.Block(bg, a, b); err != nil {
		t.Fatal(err)
	}
	if err := repo.Block(bg, a, b); err != nil { // idempotent
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := repo.Block(bg, a, c); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.Count(bg, a); err != nil || n != 2 {
		t.Fatalf("count=%d err=%v", n, err)
	}
	if ok, err := repo.IsBlocking(bg, a, b); err != nil || !ok {
		t.Fatalf("IsBlocking a->b %v %v", ok, err)
	}
	if ok, _ := repo.IsBlocking(bg, b, a); ok {
		t.Fatal("IsBlocking b->a should be false")
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		if ok, err := repo.Between(bg, pair[0], pair[1]); err != nil || !ok {
			t.Fatalf("Between %v %v %v", pair, ok, err)
		}
	}
	d := mkUser(t, p)
	if ok, _ := repo.Between(bg, a, d); ok {
		t.Fatal("Between unrelated")
	}
	got, err := repo.BlockedAmong(bg, b, []string{a, c, d, "bad"})
	if err != nil || len(got) != 1 || !got[a] {
		t.Fatalf("BlockedAmong(b) = %v err=%v", got, err)
	}
	got, err = repo.BlockedAmong(bg, a, []string{b, c, d})
	if err != nil || len(got) != 2 || !got[b] || !got[c] {
		t.Fatalf("BlockedAmong(a) = %v err=%v", got, err)
	}
	list, err := repo.List(bg, a)
	if err != nil || len(list) != 2 || list[0].UserID != c || list[1].UserID != b {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if list[1].Nickname == nil || *list[1].Nickname != "Bee" || list[1].PetType == nil || *list[1].PetType != "cat" {
		t.Fatalf("list profile %+v", list[1])
	}
	if err := repo.Unblock(bg, a, b); err != nil {
		t.Fatal(err)
	}
	if err := repo.Unblock(bg, a, b); err != nil { // not blocked any more: still fine
		t.Fatal(err)
	}
	if ok, _ := repo.Between(bg, a, b); ok {
		t.Fatal("still blocked after Unblock")
	}
}

func TestReportRepo_InsertDedupesOpen(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewReportRepo(p)
	rep, tgt := mkUser(t, p), mkUser(t, p)
	room := mkRoom(t, p, tgt)
	msg := newUUID(t, p)
	in := model.NewReport{ReporterID: rep, TargetID: tgt, RoomID: room, Reason: "spam"}

	id1, at1, err := repo.Insert(bg, in)
	if err != nil || id1 == "" {
		t.Fatalf("first %q %v", id1, err)
	}
	id2, at2, err := repo.Insert(bg, in)
	if err != nil || id2 != id1 || !at2.Equal(at1) {
		t.Fatalf("second id=%q want %q at=%v/%v err=%v", id2, id1, at2, at1, err)
	}
	if id, _, err := repo.FindOpen(bg, rep, tgt, nil); err != nil || id != id1 {
		t.Fatalf("FindOpen %q %v", id, err)
	}
	// A report on a message does not collide with the message-less one.
	in2 := in
	in2.MessageID = &msg
	id3, _, err := repo.Insert(bg, in2)
	if err != nil || id3 == id1 {
		t.Fatalf("with message id=%q err=%v", id3, err)
	}
	id4, _, err := repo.Insert(bg, in2)
	if err != nil || id4 != id3 {
		t.Fatalf("with message again id=%q err=%v", id4, err)
	}
	// Once resolved, the same report opens a new one.
	if _, err := p.Exec(bg, `update reports set status='resolved' where id=$1`, id1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.FindOpen(bg, rep, tgt, nil); err != model.ErrNotFound {
		t.Fatalf("FindOpen after resolve err=%v", err)
	}
	id5, _, err := repo.Insert(bg, in)
	if err != nil || id5 == id1 {
		t.Fatalf("after resolve id=%q err=%v", id5, err)
	}
	n, err := repo.CountSince(bg, rep, time.Now().Add(-time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("CountSince=%d err=%v", n, err)
	}
	if n, _ := repo.CountSince(bg, rep, time.Now().Add(time.Hour)); n != 0 {
		t.Fatalf("CountSince future=%d", n)
	}
}

func TestReportRepo_MessageReportable(t *testing.T) {
	p := dbtest.Reset(t)
	msgs := repository.NewMessageRepo(p)
	repo := repository.NewReportRepo(p)
	owner, u2, u3 := mkUser(t, p), mkUser(t, p), mkUser(t, p)
	room := mkRoom(t, p, owner)
	room2 := mkRoom(t, p, u3)
	seat(t, p, room, u2, 2)
	m := textMsg(t, p, room, owner, "c1")
	if ok, err := msgs.InsertMessage(bg, m); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := msgs.InsertDeliveries(bg, m.ID, []model.NewDelivery{{ReceiverID: u2, TargetLang: "ko", TranslationStatus: model.TranslationPending, PolishStatus: model.PolishNone}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.MessageReportable(bg, m.ID, room, owner, u2); err != nil || !ok {
		t.Fatalf("happy %v %v", ok, err)
	}
	cases := map[string][4]string{
		"wrong sender":    {m.ID, room, u3, u2},
		"no delivery":     {m.ID, room, owner, u3},
		"wrong room":      {m.ID, room2, owner, u2},
		"malformed id":    {"bad", room, owner, u2},
		"unknown message": {newUUID(t, p), room, owner, u2},
	}
	for name, c := range cases {
		if ok, err := repo.MessageReportable(bg, c[0], c[1], c[2], c[3]); err != nil || ok {
			t.Fatalf("%s: ok=%v err=%v", name, ok, err)
		}
	}
}

func TestModFlagRepo_Insert(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewModFlagRepo(p)
	u := mkUser(t, p)
	room, msg, del := newUUID(t, p), newUUID(t, p), newUUID(t, p)
	lang, text := "ko", "bad text"
	full := model.ModFlag{Stage: model.StageTranslation, Verdict: model.ModReview, Source: "both", Terms: []string{"a", "b"},
		UserID: u, RoomID: &room, MessageID: &msg, DeliveryID: &del, TargetLang: &lang, Text: &text}
	if err := repo.Insert(bg, full); err != nil {
		t.Fatal(err)
	}
	if err := repo.Insert(bg, model.ModFlag{Stage: model.StageNickname, Verdict: model.ModBlock, Source: "model", UserID: u}); err != nil {
		t.Fatal(err)
	}
	var terms []string
	var r, m, d, l, x *string
	var status string
	if err := p.QueryRow(bg, `select terms, room_id::text, message_id::text, delivery_id::text, target_lang, text, status from mod_flags where stage='translation'`).
		Scan(&terms, &r, &m, &d, &l, &x, &status); err != nil {
		t.Fatal(err)
	}
	if len(terms) != 2 || terms[0] != "a" || terms[1] != "b" || *r != room || *m != msg || *d != del || *l != lang || *x != text || status != "open" {
		t.Fatalf("round trip: %v %v %v %v %v %v %s", terms, r, m, d, l, x, status)
	}
	if err := p.QueryRow(bg, `select terms, room_id::text, message_id::text, delivery_id::text, target_lang, text from mod_flags where stage='nickname'`).
		Scan(&terms, &r, &m, &d, &l, &x); err != nil {
		t.Fatal(err)
	}
	if len(terms) != 0 || r != nil || m != nil || d != nil || l != nil || x != nil {
		t.Fatalf("nulls: %v %v %v %v %v %v", terms, r, m, d, l, x)
	}
}
