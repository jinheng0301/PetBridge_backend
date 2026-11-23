package repository_test

import (
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

func TestSetTranslation_WritesModStatus(t *testing.T) {
	f := newPolishFixture(t)
	id := f.delivery(t, "c1", model.NewDelivery{TranslationStatus: model.TranslationPending, PolishStatus: model.PolishNone})
	txt := "x"
	if ch, err := f.repo.SetTranslation(bg, id, &txt, model.TranslationDone, model.ModReview, time.Now()); err != nil || !ch {
		t.Fatal(ch, err)
	}
	var ms string
	f.p.QueryRow(bg, `select mod_status from deliveries where id=$1`, id).Scan(&ms)
	if ms != model.ModReview {
		t.Fatalf("mod_status=%s", ms)
	}
}

func TestPolishWork_ReadsModStatus(t *testing.T) {
	f := newPolishFixture(t)
	id := f.delivery(t, "c1", model.NewDelivery{TranslationStatus: model.TranslationDone, PolishStatus: model.PolishNone})
	if w, err := f.repo.PolishWork(bg, id); err != nil || w.ModStatus != model.ModOK {
		t.Fatalf("default %+v %v", w, err)
	}
	f.p.Exec(bg, `update deliveries set mod_status='review' where id=$1`, id)
	if w, err := f.repo.PolishWork(bg, id); err != nil || w.ModStatus != model.ModReview {
		t.Fatalf("review %+v %v", w, err)
	}
}

func TestInsertMessage_WritesModStatus(t *testing.T) {
	f := newPolishFixture(t)
	m := textMsg(t, f.p, f.room, f.owner, "c1")
	if ok, err := f.repo.InsertMessage(bg, m); err != nil || !ok {
		t.Fatal(ok, err)
	}
	m2 := textMsg(t, f.p, f.room, f.owner, "c2")
	m2.ModStatus = model.ModReview
	if ok, err := f.repo.InsertMessage(bg, m2); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for id, want := range map[string]string{m.ID: model.ModOK, m2.ID: model.ModReview} {
		var got string
		f.p.QueryRow(bg, `select mod_status from messages where id=$1`, id).Scan(&got)
		if got != want {
			t.Fatalf("%s mod_status=%s want %s", id, got, want)
		}
	}
}

func TestWithdrawMessage(t *testing.T) {
	f := newPolishFixture(t)
	u3 := mkUser(t, f.p)
	seat(t, f.p, f.room, u3, 3)
	m := textMsg(t, f.p, f.room, f.owner, "c1")
	if ok, err := f.repo.InsertMessage(bg, m); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := f.repo.InsertDeliveries(bg, m.ID, []model.NewDelivery{
		{ReceiverID: f.u2, TargetLang: "ko", TranslationStatus: model.TranslationDone, PolishStatus: model.PolishNone},
		{ReceiverID: u3, TargetLang: "en", TranslationStatus: model.TranslationPending, PolishStatus: model.PolishPending},
	}); err != nil {
		t.Fatal(err)
	}
	// Make the pending one old enough that the sweep would pick it up.
	f.p.Exec(bg, `update deliveries set updated_at = now() - interval '1 hour' where message_id=$1`, m.ID)
	now := time.Now()
	if ids, err := f.repo.StuckTranslations(bg, now, time.Minute, time.Minute, time.Hour*24, 10); err != nil || len(ids) != 1 {
		t.Fatalf("before withdraw stuck=%v err=%v", ids, err)
	}
	rids, err := f.repo.WithdrawMessage(bg, m.ID, now)
	if err != nil || len(rids) != 2 {
		t.Fatalf("rids=%v err=%v", rids, err)
	}
	got := map[string]bool{rids[0]: true, rids[1]: true}
	if !got[f.u2] || !got[u3] {
		t.Fatalf("rids=%v", rids)
	}
	rows, err := f.p.Query(bg, `select receiver_id::text, mod_status, translation_status, polish_status from deliveries where message_id=$1`, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var rid, ms, ts, ps string
		if err := rows.Scan(&rid, &ms, &ts, &ps); err != nil {
			t.Fatal(err)
		}
		n++
		want := model.PolishNone
		if rid == u3 {
			want = model.PolishFallback
		}
		if ms != model.ModBlock || ts != model.TranslationDone || ps != want {
			t.Fatalf("receiver %s: mod=%s ts=%s ps=%s", rid, ms, ts, ps)
		}
	}
	rows.Close()
	if n != 2 {
		t.Fatalf("rows=%d", n)
	}
	var mm string
	f.p.QueryRow(bg, `select mod_status from messages where id=$1`, m.ID).Scan(&mm)
	if mm != model.ModBlock {
		t.Fatalf("message mod_status=%s", mm)
	}
	if ids, err := f.repo.StuckTranslations(bg, now.Add(time.Minute), time.Minute, time.Minute, time.Hour*24, 10); err != nil || len(ids) != 0 {
		t.Fatalf("after withdraw stuck=%v err=%v", ids, err)
	}
	if rids, err := f.repo.WithdrawMessage(bg, newUUID(t, f.p), now); err != nil || len(rids) != 0 {
		t.Fatalf("unknown message %v %v", rids, err)
	}
	if rids, err := f.repo.WithdrawMessage(bg, "bad", now); err != nil || len(rids) != 0 {
		t.Fatalf("bad id %v %v", rids, err)
	}
}

func TestViewsOf_SkipsBlockedDelivery(t *testing.T) {
	f := newPolishFixture(t)
	u3 := mkUser(t, f.p)
	seat(t, f.p, f.room, u3, 3)
	m := textMsg(t, f.p, f.room, f.owner, "c1")
	f.repo.InsertMessage(bg, m)
	f.repo.InsertDeliveries(bg, m.ID, []model.NewDelivery{
		{ReceiverID: f.u2, TargetLang: "ko", TranslationStatus: model.TranslationDone, PolishStatus: model.PolishNone},
		{ReceiverID: u3, TargetLang: "en", TranslationStatus: model.TranslationDone, PolishStatus: model.PolishNone},
	})
	f.p.Exec(bg, `update deliveries set mod_status='block' where message_id=$1 and receiver_id=$2`, m.ID, f.u2)
	v, err := f.repo.ViewsOf(bg, m.ID)
	if err != nil || len(v.Receivers) != 1 {
		t.Fatalf("receivers=%v err=%v", v.Receivers, err)
	}
	if _, ok := v.Receivers[u3]; !ok {
		t.Fatalf("u3 view missing: %v", v.Receivers)
	}
}

func TestListForViewer_HidesBlockedDelivery(t *testing.T) {
	f := newPolishFixture(t)
	m := textMsg(t, f.p, f.room, f.owner, "c1")
	f.repo.InsertMessage(bg, m)
	f.repo.InsertDeliveries(bg, m.ID, []model.NewDelivery{{ReceiverID: f.u2, TargetLang: "ko", TranslationStatus: model.TranslationDone, PolishStatus: model.PolishNone}})
	items, _, err := f.repo.ListForViewer(bg, f.room, f.u2, model.PageQuery{})
	if err != nil || len(items) != 1 {
		t.Fatalf("before: %v %v", items, err)
	}
	f.p.Exec(bg, `update deliveries set mod_status='block' where message_id=$1`, m.ID)
	if items, _, err := f.repo.ListForViewer(bg, f.room, f.u2, model.PageQuery{}); err != nil || len(items) != 0 {
		t.Fatalf("receiver sees %v err=%v", items, err)
	}
	if items, _, err := f.repo.ListForViewer(bg, f.room, f.owner, model.PageQuery{}); err != nil || len(items) != 1 {
		t.Fatalf("sender sees %v err=%v", items, err)
	}
}
