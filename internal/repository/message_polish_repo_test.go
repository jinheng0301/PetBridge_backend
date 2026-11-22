package repository_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/repository"
)

func strp(s string) *string { return &s }

// polishFixture: owner sends one text message to u2 (ko, mbti ENFP); returns ids.
type polishFixture struct {
	p                      *pgxpool.Pool
	repo                   *repository.MessageRepo
	owner, u2, room, msgID string
}

func newPolishFixture(t *testing.T) polishFixture {
	t.Helper()
	p := dbtest.Reset(t)
	f := polishFixture{p: p, repo: repository.NewMessageRepo(p)}
	f.owner, f.u2 = mkUser(t, p), mkUser(t, p)
	if _, err := p.Exec(bg, `update users set mbti='ENFP' where id=$1`, f.u2); err != nil {
		t.Fatal(err)
	}
	f.room = mkRoom(t, p, f.owner)
	seat(t, p, f.room, f.u2, 2)
	return f
}

// delivery inserts a message from owner with one delivery to u2 and returns the delivery id.
func (f polishFixture) delivery(t *testing.T, cmid string, d model.NewDelivery) string {
	t.Helper()
	m := textMsg(t, f.p, f.room, f.owner, cmid)
	if ok, err := f.repo.InsertMessage(bg, m); err != nil || !ok {
		t.Fatal(ok, err)
	}
	d.ReceiverID = f.u2
	if d.TargetLang == "" {
		d.TargetLang = "ko"
	}
	if err := f.repo.InsertDeliveries(bg, m.ID, []model.NewDelivery{d}); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := f.p.QueryRow(bg, `select id::text from deliveries where message_id=$1`, m.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f polishFixture) polishState(t *testing.T, id string) (status string, by, text *string) {
	t.Helper()
	if err := f.p.QueryRow(bg, `select polish_status, polish_by::text, polished_text from deliveries where id=$1`, id).Scan(&status, &by, &text); err != nil {
		t.Fatal(err)
	}
	return
}

func TestPolishWork(t *testing.T) {
	f := newPolishFixture(t)
	id := f.delivery(t, "c1", model.NewDelivery{BaseText: strp("안녕"), TranslationStatus: model.TranslationDone,
		PolishStatus: model.PolishPending, PolishBy: &f.owner})
	w, err := f.repo.PolishWork(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	if w.DeliveryID != id || w.RoomID != f.room || w.SenderID != f.owner || w.ReceiverID != f.u2 ||
		w.Kind != model.KindText || w.SrcLang != "ja" || w.Body != "hello c1" || w.TargetLang != "ko" ||
		w.BaseText == nil || *w.BaseText != "안녕" || w.TranslationStatus != model.TranslationDone ||
		w.PolishStatus != model.PolishPending || w.PolishBy == nil || *w.PolishBy != f.owner || w.ReceiverMBTI != "ENFP" || w.MessageID == "" {
		t.Fatalf("work %+v", w)
	}
	if _, err := f.repo.PolishWork(bg, newUUID(t, f.p)); err != model.ErrNotFound {
		t.Fatalf("missing err=%v", err)
	}
	if _, err := f.repo.PolishWork(bg, "bad"); err != model.ErrNotFound {
		t.Fatalf("bad id err=%v", err)
	}
}

func TestSetPolish_OnlyPending(t *testing.T) {
	f := newPolishFixture(t)
	now := time.Now()
	id := f.delivery(t, "c1", model.NewDelivery{BaseText: strp("안녕"), TranslationStatus: model.TranslationDone,
		PolishStatus: model.PolishPending, PolishBy: &f.owner})
	if ch, err := f.repo.SetPolish(bg, id, strp("안녕하세요~"), model.PolishDone, now); err != nil || !ch {
		t.Fatalf("done ch=%v err=%v", ch, err)
	}
	if ch, err := f.repo.SetPolish(bg, id, nil, model.PolishFallback, now); err != nil || ch {
		t.Fatalf("regress ch=%v err=%v", ch, err)
	}
	st, _, txt := f.polishState(t, id)
	if st != model.PolishDone || txt == nil || *txt != "안녕하세요~" {
		t.Fatalf("st=%s txt=%v", st, txt)
	}
	none := f.delivery(t, "c2", model.NewDelivery{TranslationStatus: model.TranslationDone, PolishStatus: model.PolishNone})
	if ch, err := f.repo.SetPolish(bg, none, nil, model.PolishFallback, now); err != nil || ch {
		t.Fatalf("none row ch=%v err=%v", ch, err)
	}
}

func TestClaimPolish(t *testing.T) {
	f := newPolishFixture(t)
	now := time.Now()
	id := f.delivery(t, "c1", model.NewDelivery{BaseText: strp("안녕"), TranslationStatus: model.TranslationDone, PolishStatus: model.PolishNone})
	if ok, err := f.repo.ClaimPolish(bg, id, f.owner, now); err != nil || ok {
		t.Fatalf("non-receiver claim ok=%v err=%v", ok, err)
	}
	if ok, err := f.repo.ClaimPolish(bg, id, f.u2, now); err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if ok, err := f.repo.ClaimPolish(bg, id, f.u2, now); err != nil || ok {
		t.Fatalf("second claim ok=%v err=%v", ok, err)
	}
	st, by, _ := f.polishState(t, id)
	if st != model.PolishPending || by == nil || *by != f.u2 {
		t.Fatalf("after claim st=%s by=%v", st, by)
	}
	if ok, err := f.repo.ClaimPolish(bg, "bad", f.u2, now); err != nil || ok {
		t.Fatalf("bad id ok=%v err=%v", ok, err)
	}
}

func TestStuckPolishes(t *testing.T) {
	f := newPolishFixture(t)
	now := time.Now()
	age := func(id string, d time.Duration) {
		if _, err := f.p.Exec(bg, `update deliveries set updated_at=$2 where id=$1`, id, now.Add(-d)); err != nil {
			t.Fatal(err)
		}
	}
	stuck := f.delivery(t, "a", model.NewDelivery{BaseText: strp("x"), TranslationStatus: model.TranslationDone, PolishStatus: model.PolishPending, PolishBy: &f.owner})
	age(stuck, 3*time.Minute)
	fresh := f.delivery(t, "b", model.NewDelivery{BaseText: strp("x"), TranslationStatus: model.TranslationDone, PolishStatus: model.PolishPending, PolishBy: &f.owner})
	age(fresh, time.Minute)
	waiting := f.delivery(t, "c", model.NewDelivery{TranslationStatus: model.TranslationPending, PolishStatus: model.PolishPending, PolishBy: &f.owner})
	age(waiting, time.Hour)
	done := f.delivery(t, "d", model.NewDelivery{BaseText: strp("x"), TranslationStatus: model.TranslationDone, PolishStatus: model.PolishDone, PolishBy: &f.owner})
	age(done, time.Hour)

	got, err := f.repo.StuckPolishes(bg, now, 2*time.Minute, 10)
	if err != nil || len(got) != 1 || got[0] != stuck {
		t.Fatalf("got %v err=%v want [%s]", got, err, stuck)
	}
}

func TestGiveUpPolishes(t *testing.T) {
	f := newPolishFixture(t)
	now := time.Now()
	mk := func(cm, tr string, msgAge time.Duration) string {
		id := f.delivery(t, cm, model.NewDelivery{TranslationStatus: tr, PolishStatus: model.PolishPending, PolishBy: &f.owner})
		if _, err := f.p.Exec(bg, `update messages set created_at=$2 where id=(select message_id from deliveries where id=$1)`, id, now.Add(-msgAge)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := mk("a", model.TranslationFailed, 25*time.Hour)
	young := mk("b", model.TranslationFailed, 23*time.Hour)
	doneTr := mk("c", model.TranslationDone, 25*time.Hour)

	got, err := f.repo.GiveUpPolishes(bg, now, 24*time.Hour, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	g := got[0]
	if g.DeliveryID != old || g.RoomID != f.room || g.ReceiverID != f.u2 || g.MessageID == "" {
		t.Fatalf("give-up row %+v", g)
	}
	if st, _, _ := f.polishState(t, old); st != model.PolishFallback {
		t.Fatalf("old st=%s", st)
	}
	for _, id := range []string{young, doneTr} {
		if st, _, _ := f.polishState(t, id); st != model.PolishPending {
			t.Fatalf("%s st=%s", id, st)
		}
	}
	if again, _ := f.repo.GiveUpPolishes(bg, now, 24*time.Hour, 10); len(again) != 0 {
		t.Fatalf("second run %+v", again)
	}
}

func TestInsertDeliveries_PolishedText(t *testing.T) {
	f := newPolishFixture(t)
	id := f.delivery(t, "c1", model.NewDelivery{BaseText: strp("OK!"), TranslationStatus: model.TranslationDone,
		PolishStatus: model.PolishDone, PolishBy: &f.owner, PolishedText: strp("OK!")})
	var mid string
	f.p.QueryRow(bg, `select message_id::text from deliveries where id=$1`, id).Scan(&mid)
	views, err := f.repo.ViewsOf(bg, mid)
	if err != nil {
		t.Fatal(err)
	}
	d := views.Receivers[f.u2].Delivery
	if d == nil || d.PolishedText == nil || *d.PolishedText != "OK!" || d.PolishStatus != model.PolishDone {
		t.Fatalf("delivery %+v", d)
	}
}

func TestTranslationWork_SenderAndPolishStatus(t *testing.T) {
	f := newPolishFixture(t)
	id := f.delivery(t, "c1", model.NewDelivery{TranslationStatus: model.TranslationPending, PolishStatus: model.PolishPending, PolishBy: &f.owner})
	var mid string
	f.p.QueryRow(bg, `select message_id::text from deliveries where id=$1`, id).Scan(&mid)
	w, err := f.repo.TranslationWork(bg, mid)
	if err != nil || w.SenderID != f.owner || len(w.Pending) != 1 || w.Pending[0].PolishStatus != model.PolishPending {
		t.Fatalf("work %+v err=%v", w, err)
	}
}
