package repository_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/service"
)

var _ service.MessageStore = (*repository.MessageRepo)(nil)

func newUUID(t *testing.T, p *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := p.QueryRow(bg, `select gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func seat(t *testing.T, p *pgxpool.Pool, room, user string, seatNo int) {
	t.Helper()
	if _, err := p.Exec(bg, `insert into room_members(room_id,user_id,seat_no,role) values($1,$2,$3,'member')`, room, user, seatNo); err != nil {
		t.Fatal(err)
	}
}

func textMsg(t *testing.T, p *pgxpool.Pool, room, sender, cmid string) model.NewMessage {
	t.Helper()
	return model.NewMessage{ID: newUUID(t, p), RoomID: room, SenderID: sender, Scope: model.ScopeRoom, Kind: model.KindText,
		SrcLang: "ja", Body: "hello " + cmid, ClientMsgID: cmid}
}

func TestMessageRepo_InsertIdempotent(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewMessageRepo(p)
	owner := mkUser(t, p)
	room := mkRoom(t, p, owner)
	m := textMsg(t, p, room, owner, "c1")
	ok, err := repo.InsertMessage(bg, m)
	if err != nil || !ok {
		t.Fatalf("first insert ok=%v err=%v", ok, err)
	}
	m2 := textMsg(t, p, room, owner, "c1")
	ok, err = repo.InsertMessage(bg, m2)
	if err != nil || ok {
		t.Fatalf("second insert ok=%v err=%v", ok, err)
	}
	id, err := repo.FindByClientMsgID(bg, owner, "c1")
	if err != nil || id != m.ID {
		t.Fatalf("find id=%q err=%v want %q", id, err, m.ID)
	}
	if _, err := repo.FindByClientMsgID(bg, owner, "nope"); err != model.ErrNotFound {
		t.Fatalf("err=%v", err)
	}
	if _, err := repo.FindByClientMsgID(bg, "bad", "c1"); err != model.ErrNotFound {
		t.Fatalf("bad uuid err=%v", err)
	}
}

func TestMessageRepo_ActiveRecipients(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewMessageRepo(p)
	owner, u2, u3 := mkUser(t, p), mkUser(t, p), mkUser(t, p)
	room := mkRoom(t, p, owner)
	seat(t, p, room, u2, 2)
	seat(t, p, room, u3, 3)
	p.Exec(bg, `update users set native_lang='ko', age_band='13_17' where id=$1`, u2)
	p.Exec(bg, `update room_members set left_at=now(), left_reason='left' where room_id=$1 and user_id=$2`, room, u3)
	rs, err := repo.ActiveRecipients(bg, room)
	if err != nil || len(rs) != 2 {
		t.Fatalf("rs=%+v err=%v", rs, err)
	}
	for _, r := range rs {
		if r.UserID == u2 && (r.NativeLang != "ko" || r.AgeBand != "13_17") {
			t.Fatalf("u2=%+v", r)
		}
		if r.UserID == owner && (r.NativeLang != "" || r.AgeBand != "") {
			t.Fatalf("owner=%+v", r)
		}
	}
}

func TestMessageRepo_ViewsOf(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewMessageRepo(p)
	owner, u2 := mkUser(t, p), mkUser(t, p)
	room := mkRoom(t, p, owner)
	seat(t, p, room, u2, 2)
	p.Exec(bg, `update users set nickname='Mochi', pet_type='cat' where id=$1`, owner)
	m := textMsg(t, p, room, owner, "c1")
	if ok, err := repo.InsertMessage(bg, m); err != nil || !ok {
		t.Fatal(ok, err)
	}
	base := "こんにちは"
	err := repo.InsertDeliveries(bg, m.ID, []model.NewDelivery{
		{ReceiverID: u2, TargetLang: "ko", BaseText: &base, TranslationStatus: model.TranslationDone, PolishStatus: model.PolishNone},
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := repo.ViewsOf(bg, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Sender.Delivery != nil || v.Sender.ClientMsgID == nil || *v.Sender.ClientMsgID != "c1" {
		t.Fatalf("sender view %+v", v.Sender)
	}
	if v.Sender.Sender.Nickname == nil || *v.Sender.Sender.Nickname != "Mochi" || *v.Sender.Sender.PetType != "cat" {
		t.Fatalf("sender info %+v", v.Sender.Sender)
	}
	rv, ok := v.Receivers[u2]
	if !ok || len(v.Receivers) != 1 {
		t.Fatalf("receivers %+v", v.Receivers)
	}
	var did string
	p.QueryRow(bg, `select id::text from deliveries where message_id=$1 and receiver_id=$2`, m.ID, u2).Scan(&did)
	if rv.ClientMsgID != nil || rv.Delivery == nil || rv.Delivery.ID != did || rv.Delivery.BaseText == nil || *rv.Delivery.BaseText != base ||
		rv.Delivery.TargetLang != "ko" || rv.Sender.Nickname == nil || *rv.Sender.Nickname != "Mochi" {
		t.Fatalf("receiver view %+v / %+v", rv, rv.Delivery)
	}
	if _, err := repo.ViewsOf(bg, newUUID(t, p)); err != model.ErrNotFound {
		t.Fatalf("err=%v", err)
	}
	if _, err := repo.ViewsOf(bg, "bad"); err != model.ErrNotFound {
		t.Fatalf("err=%v", err)
	}
}

func idsOf(items []model.MessageView) []string {
	var o []string
	for _, i := range items {
		o = append(o, i.ID)
	}
	return o
}

func TestMessageRepo_ListForViewer(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewMessageRepo(p)
	a, b, c := mkUser(t, p), mkUser(t, p), mkUser(t, p)
	room := mkRoom(t, p, a)
	seat(t, p, room, b, 2)
	seat(t, p, room, c, 3)
	var dmID string
	for i := 1; i <= 5; i++ {
		m := textMsg(t, p, room, a, fmt.Sprintf("c%d", i))
		ds := []model.NewDelivery{{ReceiverID: b, TargetLang: "ko", TranslationStatus: model.TranslationPending, PolishStatus: model.PolishNone}}
		if i == 2 { // DM from a to c, invisible to b
			m.Scope, m.ToUserID = model.ScopeDM, &c
			ds = []model.NewDelivery{{ReceiverID: c, TargetLang: "ja", TranslationStatus: model.TranslationPending, PolishStatus: model.PolishNone}}
			dmID = m.ID
		}
		if ok, err := repo.InsertMessage(bg, m); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := repo.InsertDeliveries(bg, m.ID, ds); err != nil {
			t.Fatal(err)
		}
	}
	// Message ids are random uuids, so order them the way the repository does.
	var sorted []string
	rows, err := p.Query(bg, `select id::text from messages order by id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		rows.Scan(&s)
		sorted = append(sorted, s)
	}
	rows.Close()
	var vis []string // what b can see
	for _, s := range sorted {
		if s != dmID {
			vis = append(vis, s)
		}
	}
	items, more, err := repo.ListForViewer(bg, room, b, model.PageQuery{Limit: 2})
	if err != nil || !more || len(items) != 2 || items[0].ID != vis[2] || items[1].ID != vis[3] {
		t.Fatalf("latest: %v %v err=%v", idsOf(items), more, err)
	}
	if items[0].Delivery == nil || items[0].ClientMsgID != nil {
		t.Fatalf("receiver view expected: %+v", items[0])
	}
	items, more, err = repo.ListForViewer(bg, room, b, model.PageQuery{Limit: 2, Before: vis[2]})
	if err != nil || len(items) != 2 || items[0].ID != vis[0] || items[1].ID != vis[1] || more {
		t.Fatalf("before: %v %v err=%v", idsOf(items), more, err)
	}
	items, more, err = repo.ListForViewer(bg, room, b, model.PageQuery{Limit: 5, After: vis[1]})
	if err != nil || len(items) != 2 || items[0].ID != vis[2] || items[1].ID != vis[3] || more {
		t.Fatalf("after: %v %v err=%v", idsOf(items), more, err)
	}
	items, _, err = repo.ListForViewer(bg, room, a, model.PageQuery{Limit: 10})
	if err != nil || len(items) != 5 || items[0].Delivery != nil || items[0].ClientMsgID == nil {
		t.Fatalf("sender list %v err=%v", idsOf(items), err)
	}
	items, _, err = repo.ListForViewer(bg, room, c, model.PageQuery{Limit: 10})
	if err != nil || len(items) != 1 || items[0].ID != dmID {
		t.Fatalf("c list %v err=%v", idsOf(items), err)
	}
}

func TestMessageRepo_SetTranslationNeverRegresses(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewMessageRepo(p)
	owner, u2 := mkUser(t, p), mkUser(t, p)
	room := mkRoom(t, p, owner)
	seat(t, p, room, u2, 2)
	m := textMsg(t, p, room, owner, "c1")
	repo.InsertMessage(bg, m)
	repo.InsertDeliveries(bg, m.ID, []model.NewDelivery{{ReceiverID: u2, TargetLang: "ko", TranslationStatus: model.TranslationPending, PolishStatus: model.PolishNone}})
	var did string
	p.QueryRow(bg, `select id::text from deliveries where message_id=$1`, m.ID).Scan(&did)
	now := time.Now()

	w, err := repo.TranslationWork(bg, m.ID)
	if err != nil || len(w.Pending) != 1 || w.Pending[0].ID != did || w.Body != m.Body || w.SrcLang != "ja" || w.RoomID != room {
		t.Fatalf("work %+v err=%v", w, err)
	}
	fail := model.TranslationFailed
	if ch, err := repo.SetTranslation(bg, did, nil, fail, now); err != nil || !ch {
		t.Fatalf("failed write ch=%v err=%v", ch, err)
	}
	txt := "안녕"
	if ch, err := repo.SetTranslation(bg, did, &txt, model.TranslationDone, now); err != nil || !ch {
		t.Fatalf("done write ch=%v err=%v", ch, err)
	}
	if ch, err := repo.SetTranslation(bg, did, nil, fail, now); err != nil || ch {
		t.Fatalf("regress ch=%v err=%v", ch, err)
	}
	var st string
	var base *string
	var att int
	p.QueryRow(bg, `select translation_status, base_text, translate_attempts from deliveries where id=$1`, did).Scan(&st, &base, &att)
	if st != "done" || base == nil || *base != txt || att != 2 {
		t.Fatalf("st=%s base=%v att=%d", st, base, att)
	}
	w, err = repo.TranslationWork(bg, m.ID)
	if err != nil || len(w.Pending) != 0 || w.MessageID != m.ID {
		t.Fatalf("work after %+v err=%v", w, err)
	}
	if _, err := repo.TranslationWork(bg, newUUID(t, p)); err != model.ErrNotFound {
		t.Fatalf("err=%v", err)
	}
	if ch, err := repo.SetTranslation(bg, "bad", nil, fail, now); err != nil || ch {
		t.Fatalf("bad id ch=%v err=%v", ch, err)
	}
}

func TestMessageRepo_StuckTranslations(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewMessageRepo(p)
	owner, u2 := mkUser(t, p), mkUser(t, p)
	room := mkRoom(t, p, owner)
	seat(t, p, room, u2, 2)
	now := time.Now()
	mk := func(cm, status string, msgAge, updAge time.Duration) string {
		m := textMsg(t, p, room, owner, cm)
		if ok, err := repo.InsertMessage(bg, m); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if err := repo.InsertDeliveries(bg, m.ID, []model.NewDelivery{{ReceiverID: u2, TargetLang: "ko", TranslationStatus: status, PolishStatus: model.PolishNone}}); err != nil {
			t.Fatal(err)
		}
		p.Exec(bg, `update messages set created_at=$2 where id=$1`, m.ID, now.Add(-msgAge))
		p.Exec(bg, `update deliveries set updated_at=$2 where message_id=$1`, m.ID, now.Add(-updAge))
		return m.ID
	}
	pendOld := mk("a", "pending", 4*time.Minute, 3*time.Minute)
	mk("b", "pending", 2*time.Minute, time.Minute)
	failRetry := mk("c", "failed", time.Hour, 11*time.Minute)
	mk("d", "failed", 25*time.Hour, 11*time.Minute)
	mk("e", "done", time.Hour, time.Hour)
	mk("f", "failed", time.Hour, time.Minute)

	got, err := repo.StuckTranslations(bg, now, 2*time.Minute, 10*time.Minute, 24*time.Hour, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v err=%v", got, err)
	}
	want := map[string]bool{pendOld: true, failRetry: true}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected %s", id)
		}
	}
	got, _ = repo.StuckTranslations(bg, now, 2*time.Minute, 10*time.Minute, 24*time.Hour, 1)
	if len(got) != 1 {
		t.Fatalf("limit: %v", got)
	}
}

func TestMessageRepo_ListForViewer_LimitDefaultAndMax(t *testing.T) {
	p := dbtest.Reset(t)
	repo := repository.NewMessageRepo(p)
	a := mkUser(t, p)
	room := mkRoom(t, p, a)
	for i := 0; i < 105; i++ {
		if ok, err := repo.InsertMessage(bg, textMsg(t, p, room, a, fmt.Sprintf("c%d", i))); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	items, more, err := repo.ListForViewer(bg, room, a, model.PageQuery{})
	if err != nil || len(items) != model.DefaultPageLimit || !more {
		t.Fatalf("default: n=%d more=%v err=%v", len(items), more, err)
	}
	items, more, err = repo.ListForViewer(bg, room, a, model.PageQuery{Limit: 1000})
	if err != nil || len(items) != model.MaxPageLimit || !more {
		t.Fatalf("max: n=%d more=%v err=%v", len(items), more, err)
	}
}
