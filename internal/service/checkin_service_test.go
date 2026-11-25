package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/repository"
)

// checkinEnv is a joinEnv with a check-in service on its own clock; every
// onboarded user lives in Asia/Tokyo.
type checkinEnv struct {
	*joinEnv
	store *repository.CheckinRepo
	at    time.Time
}

func newCheckinEnv(t *testing.T) *checkinEnv {
	t.Helper()
	e := &checkinEnv{joinEnv: newJoinEnv(t), at: mustUTC(t, "2026-11-15T12:00:00Z")}
	e.store = repository.NewCheckinRepo(e.pool)
	return e
}

func (e *checkinEnv) svcTx(tx TxRunner) *CheckinService {
	return NewCheckinService(tx, repository.NewUserRepo(e.pool), repository.NewRoomRepo(e.pool),
		repository.NewRoomMemberRepo(e.pool), e.store, e.pub, func() time.Time { return e.at })
}

// checkin checks user in to room at the given time (RFC 3339; "" keeps the clock).
func (e *checkinEnv) checkin(t *testing.T, user, room, at string) model.CheckinResult {
	t.Helper()
	if at != "" {
		e.at = mustUTC(t, at)
	}
	res, err := e.svcTx(e.txm).Checkin(bg, user, room)
	if err != nil {
		t.Fatalf("check-in: %v", err)
	}
	return res
}

// pair is an owner and a member seated in the owner's room.
func (e *checkinEnv) pair(t *testing.T) (a, b, room string) {
	t.Helper()
	a = e.user(t, "a@x.test", "18_plus")
	b = e.user(t, "b@x.test", "18_plus")
	e.mustJoin(t, b, e.extraCode(t, a))
	e.pub.reset()
	return a, b, e.roomOf(t, a)
}

func (e *checkinEnv) energy(t *testing.T, room string) int {
	t.Helper()
	n, err := e.store.Energy(bg, room)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func wantThreshold(t *testing.T, got *int, want int) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("next threshold %v want %d", got, want)
	}
}

func TestCheckin_First(t *testing.T) {
	e := newCheckinEnv(t)
	u := e.user(t, "a@x.test", "18_plus")
	room := e.roomOf(t, u)
	e.pub.reset()
	res := e.checkin(t, u, room, "")
	midnight := mustUTC(t, "2026-11-15T15:00:00Z")
	if res.Already || res.EnergyGained != 10 || res.BonusGained != 0 || res.EnergyTotal != 10 ||
		!slices.Equal(res.Unlocked, []string{"wall_clock"}) || !res.CheckinExpiresAt.Equal(midnight) {
		t.Fatalf("result %+v", res)
	}
	wantThreshold(t, res.NextThreshold, 40)

	evs := e.pub.all()
	if len(evs) != 2 || evs[0].Type != EventCheckinUpdated || evs[1].Type != EventDecorUnlocked {
		t.Fatalf("events %+v", evs)
	}
	for _, ev := range evs {
		if ev.Kind != KindTransient || ev.RoomID != room || !slices.Equal(ev.To, []string{u}) || ev.Data["room_id"] != room {
			t.Fatalf("event %+v", ev)
		}
	}
	d := evs[0].Data
	if d["user_id"] != u || d["energy_total"] != 10 || d["energy_gained"] != 10 || d["bonus_gained"] != 0 ||
		!d["checkin_expires_at"].(time.Time).Equal(midnight) || len(d) != 7 {
		t.Fatalf("checkin.updated %+v", d)
	}
	wantThreshold(t, d["next_threshold"].(*int), 40)
	d = evs[1].Data
	if d["item_id"] != "wall_clock" || !d["unlocked_at"].(time.Time).Equal(e.at) || len(d) != 3 {
		t.Fatalf("decor.unlocked %+v", d)
	}
}

func TestCheckin_AlreadySameDay(t *testing.T) {
	e := newCheckinEnv(t)
	u := e.user(t, "a@x.test", "18_plus")
	room := e.roomOf(t, u)
	e.checkin(t, u, room, "")
	e.pub.reset()
	res := e.checkin(t, u, room, "2026-11-15T14:59:59Z")
	if !res.Already || res.EnergyGained != 0 || res.BonusGained != 0 || res.EnergyTotal != 10 ||
		res.Unlocked == nil || len(res.Unlocked) != 0 || !res.CheckinExpiresAt.Equal(mustUTC(t, "2026-11-15T15:00:00Z")) {
		t.Fatalf("repeat %+v", res)
	}
	wantThreshold(t, res.NextThreshold, 40)
	if n := len(e.pub.all()); n != 0 {
		t.Fatalf("%d events on a repeat", n)
	}
	// Tokyo midnight.
	if res := e.checkin(t, u, room, "2026-11-15T15:00:00Z"); res.Already || res.EnergyTotal != 20 {
		t.Fatalf("next day %+v", res)
	}
}

func TestCheckin_NotSeated(t *testing.T) {
	e := newCheckinEnv(t)
	a, b, room := e.pair(t)
	stranger := e.user(t, "s@x.test", "18_plus")
	if _, err := e.mgr.Remove(bg, a, room, b); err != nil {
		t.Fatal(err)
	}
	svc := e.svcTx(e.txm)
	for _, c := range []struct{ user, room string }{
		{stranger, room}, {b, room}, {a, "00000000-0000-0000-0000-000000000000"}, {a, "abc"},
	} {
		if _, err := svc.Checkin(bg, c.user, c.room); !errors.Is(err, ErrRoomNotFound) {
			t.Fatalf("Checkin(%s, %q) err=%v", c.user, c.room, err)
		}
	}
	if n := count(t, e.pool, `select count(*) from checkins`); n != 0 {
		t.Fatalf("%d rows", n)
	}
}

func TestCheckin_UppercaseRoomID(t *testing.T) {
	e := newCheckinEnv(t)
	u := e.user(t, "a@x.test", "18_plus")
	room := e.roomOf(t, u)
	e.pub.reset()
	e.checkin(t, u, strings.ToUpper(room), "")
	evs := e.pub.ofType(EventCheckinUpdated)
	if len(evs) != 1 || evs[0].RoomID != room || evs[0].Data["room_id"] != room {
		t.Fatalf("events %+v", evs)
	}
}

func TestCheckin_TwoRoomsIndependent(t *testing.T) {
	e := newCheckinEnv(t)
	_, b, room := e.pair(t)
	own := e.roomOf(t, b)
	for _, r := range []string{room, own} {
		if res := e.checkin(t, b, r, ""); res.Already || res.EnergyTotal != 10 {
			t.Fatalf("room %s: %+v", r, res)
		}
	}
}

func TestCheckin_FullHouse(t *testing.T) {
	e := newCheckinEnv(t)
	a, b, room := e.pair(t)
	if res := e.checkin(t, a, room, ""); res.BonusGained != 0 {
		t.Fatalf("first %+v", res)
	}
	e.pub.reset()
	res := e.checkin(t, b, room, "2026-11-15T13:00:00Z")
	if res.EnergyGained != 10 || res.BonusGained != 20 || res.EnergyTotal != 40 || !slices.Equal(res.Unlocked, []string{"picture_frame"}) {
		t.Fatalf("second %+v", res)
	}
	evs := e.pub.all()
	if len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	for _, ev := range evs {
		if len(ev.To) != 2 || !slices.Contains(ev.To, a) || !slices.Contains(ev.To, b) {
			t.Fatalf("to %v", ev.To)
		}
	}
	if evs[0].Data["bonus_gained"] != 20 || evs[0].Data["energy_gained"] != 10 {
		t.Fatalf("data %+v", evs[0].Data)
	}
}

func TestCheckin_FullHouseFivePeople(t *testing.T) {
	e := newCheckinEnv(t)
	owner := e.user(t, "a@x.test", "18_plus")
	room := e.roomOf(t, owner)
	people := append([]string{owner}, e.seat(t, owner, 4)...)
	for i, u := range people {
		want := 0
		if i == 4 {
			want = 20
		}
		if res := e.checkin(t, u, room, ""); res.BonusGained != want {
			t.Fatalf("person %d bonus %d want %d", i, res.BonusGained, want)
		}
	}
	if n := e.energy(t, room); n != 70 {
		t.Fatalf("energy %d", n)
	}
}

func TestCheckin_FullHouseOncePerRound(t *testing.T) {
	e := newCheckinEnv(t)
	a, b, room := e.pair(t)
	for _, c := range []struct {
		user, at string
		bonus    int
	}{
		{a, "2026-11-15T00:00:00Z", 0},
		{b, "2026-11-15T11:00:00Z", 20},
		{a, "2026-11-16T00:00:00Z", 0}, // b's check-in is within 24h but already used
		{b, "2026-11-16T11:00:00Z", 20},
	} {
		if res := e.checkin(t, c.user, room, c.at); res.BonusGained != c.bonus {
			t.Fatalf("%s: bonus %d want %d", c.at, res.BonusGained, c.bonus)
		}
	}
	if n := e.energy(t, room); n != 80 {
		t.Fatalf("energy %d", n)
	}
}

func TestCheckin_NewMemberMustCheckInToo(t *testing.T) {
	e := newCheckinEnv(t)
	a, b, room := e.pair(t)
	e.checkin(t, a, room, "")
	c := e.user(t, "c@x.test", "18_plus")
	e.mustJoin(t, c, e.extraCode(t, a))
	if res := e.checkin(t, b, room, ""); res.BonusGained != 0 {
		t.Fatalf("b %+v", res)
	}
	if res := e.checkin(t, c, room, ""); res.BonusGained != 20 {
		t.Fatalf("c %+v", res)
	}
}

func TestCheckin_LeaveDoesNotPayUntilNextCheckin(t *testing.T) {
	e := newCheckinEnv(t)
	a, b, room := e.pair(t)
	c := e.user(t, "c@x.test", "18_plus")
	e.mustJoin(t, c, e.extraCode(t, a))
	e.checkin(t, a, room, "2026-11-15T12:00:00Z")
	e.checkin(t, b, room, "2026-11-15T13:00:00Z")
	e.pub.reset()
	if err := e.mgr.Leave(bg, c, room); err != nil {
		t.Fatal(err)
	}
	if n := e.energy(t, room); n != 20 || len(e.pub.ofType(EventCheckinUpdated)) != 0 {
		t.Fatalf("leaving paid: energy %d", n)
	}
	if res := e.checkin(t, a, room, "2026-11-15T15:30:00Z"); res.BonusGained != 20 {
		t.Fatalf("next check-in %+v", res)
	}
}

func TestCheckin_RejoinSameDay(t *testing.T) {
	e := newCheckinEnv(t)
	a, b, room := e.pair(t)
	e.checkin(t, b, room, "")
	if err := e.mgr.Leave(bg, b, room); err != nil {
		t.Fatal(err)
	}
	e.mustJoin(t, b, e.extraCode(t, a))
	if res := e.checkin(t, b, room, "2026-11-15T13:00:00Z"); !res.Already || res.EnergyTotal != 10 {
		t.Fatalf("rejoined %+v", res)
	}
	if res := e.checkin(t, a, room, ""); res.BonusGained != 20 {
		t.Fatalf("the earlier check-in did not count: %+v", res)
	}
}

func TestCheckin_CrossesSeveralThresholds(t *testing.T) {
	e := newCheckinEnv(t)
	u := e.user(t, "a@x.test", "18_plus")
	room := e.roomOf(t, u)
	if _, err := e.store.AddEnergy(bg, room, 300, e.at); err != nil {
		t.Fatal(err)
	}
	e.pub.reset()
	res := e.checkin(t, u, room, "")
	want := []string{"wall_clock", "picture_frame", "floor_lamp", "string_lights", "potted_tree"}
	if res.EnergyTotal != 310 || !slices.Equal(res.Unlocked, want) {
		t.Fatalf("result %+v", res)
	}
	var items []string
	for _, ev := range e.pub.ofType(EventDecorUnlocked) {
		items = append(items, ev.Data["item_id"].(string))
	}
	if !slices.Equal(items, want) {
		t.Fatalf("events %v", items)
	}

	v := e.user(t, "b@x.test", "18_plus")
	room = e.roomOf(t, v)
	if _, err := e.store.AddEnergy(bg, room, 1335, e.at); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.UnlockDecor(bg, room, ItemsReached(1335), e.at); err != nil {
		t.Fatal(err)
	}
	res = e.checkin(t, v, room, "")
	if res.NextThreshold != nil || !slices.Equal(res.Unlocked, []string{"wind_chime"}) {
		t.Fatalf("last %+v", res)
	}
}

func TestCheckin_KeepsProgressAfterDissolve(t *testing.T) {
	e := newCheckinEnv(t)
	a, b, room := e.pair(t)
	e.checkin(t, a, room, "")
	e.checkin(t, b, room, "")
	if _, err := e.mgr.Dissolve(bg, a, room); err != nil {
		t.Fatal(err)
	}
	decor, err := e.store.ListDecor(bg, room)
	if err != nil || len(decor) != 2 || e.energy(t, room) != 40 {
		t.Fatalf("after dissolve: decor %v energy %d err=%v", decor, e.energy(t, room), err)
	}
	if res := e.checkin(t, a, room, "2026-11-16T12:00:00Z"); res.EnergyTotal != 50 {
		t.Fatalf("next day %+v", res)
	}
}

func TestCheckin_RollsBackTogether(t *testing.T) {
	e := newCheckinEnv(t)
	u := e.user(t, "a@x.test", "18_plus")
	room := e.roomOf(t, u)
	e.pub.reset()
	if _, err := e.svcTx(commitFailTx{e.txm}).Checkin(bg, u, room); !errors.Is(err, errCommitFailed) {
		t.Fatalf("err=%v", err)
	}
	n := count(t, e.pool, `select (select count(*) from checkins)+(select count(*) from room_progress)+(select count(*) from room_decor)`)
	if n != 0 || len(e.pub.all()) != 0 {
		t.Fatalf("%d rows, %d events", n, len(e.pub.all()))
	}
}

func TestCheckin_PublishesAfterClientGone(t *testing.T) {
	e := newCheckinEnv(t)
	u := e.user(t, "a@x.test", "18_plus")
	room := e.roomOf(t, u)
	e.pub.reset()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	if _, err := e.svcTx(cancelAfterTx{e.txm, cancel}).Checkin(ctx, u, room); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.all()) != 2 || e.pub.deadCalls != 0 {
		t.Fatalf("%d events, %d on a dead context", len(e.pub.all()), e.pub.deadCalls)
	}
	if res := e.checkin(t, u, room, ""); !res.Already || res.EnergyTotal != 10 {
		t.Fatalf("retry %+v", res)
	}
}

func TestDecor_List(t *testing.T) {
	e := newCheckinEnv(t)
	u := e.user(t, "a@x.test", "18_plus")
	stranger := e.user(t, "s@x.test", "18_plus")
	room := e.roomOf(t, u)
	svc := e.svcTx(e.txm)
	l, err := svc.Decor(bg, u, room)
	if err != nil || l.EnergyTotal != 0 || len(l.Items) != 12 {
		t.Fatalf("fresh %+v err=%v", l, err)
	}
	wantThreshold(t, l.NextThreshold, 10)
	for i, d := range DecorCatalog() {
		if l.Items[i].ItemID != d.ID || l.Items[i].Threshold != d.Threshold || l.Items[i].UnlockedAt != nil {
			t.Fatalf("item %d %+v", i, l.Items[i])
		}
	}
	e.checkin(t, u, room, "")
	l, err = svc.Decor(bg, u, strings.ToUpper(room))
	if err != nil || l.EnergyTotal != 10 || l.Items[0].UnlockedAt == nil || !l.Items[0].UnlockedAt.Equal(e.at) || l.Items[1].UnlockedAt != nil {
		t.Fatalf("after check-in %+v err=%v", l, err)
	}
	for _, c := range []struct{ user, room string }{{stranger, room}, {u, "abc"}} {
		if _, err := svc.Decor(bg, c.user, c.room); !errors.Is(err, ErrRoomNotFound) {
			t.Fatalf("Decor(%s, %q) err=%v", c.user, c.room, err)
		}
	}
}

func TestDecor_UnknownItemIgnored(t *testing.T) {
	e := newCheckinEnv(t)
	u := e.user(t, "a@x.test", "18_plus")
	room := e.roomOf(t, u)
	if _, err := e.store.UnlockDecor(bg, room, []string{"old_item"}, e.at); err != nil {
		t.Fatal(err)
	}
	l, err := e.svcTx(e.txm).Decor(bg, u, room)
	if err != nil || len(l.Items) != 12 {
		t.Fatalf("%+v err=%v", l, err)
	}
	for _, it := range l.Items {
		if it.ItemID == "old_item" {
			t.Fatal("unknown item listed")
		}
	}
}
