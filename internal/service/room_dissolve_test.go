package service

import (
	"errors"
	"slices"
	"testing"
)

func TestDissolve_Success(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	m := e.fill(t, owner, 3)
	c1, c2 := e.extraCode(t, owner), e.extraCode(t, owner)
	signup := e.signupCode(t, owner)
	e.pub.reset()

	res, err := e.mgr.Dissolve(bg, owner, room)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != room || res.MemberCount != 1 || res.Status != "open" || res.Capacity != 5 {
		t.Fatalf("summary %+v", res)
	}
	for _, c := range []string{c1, c2, signup} {
		if st := e.codeStatus(t, c); st != "void" {
			t.Fatalf("code %s status=%s want void", c, st)
		}
	}
	for _, u := range m {
		var reason string
		if err := e.pool.QueryRow(bg, `select left_reason from room_members where room_id=$1 and user_id=$2`, room, u).Scan(&reason); err != nil || reason != "dissolved" {
			t.Fatalf("member %s left_reason=%q err=%v", u, reason, err)
		}
	}
	if ok, _ := e.mgr.members.IsActiveMember(bg, room, owner); !ok {
		t.Fatal("owner must stay seated")
	}
	if n := count(t, e.pool, `select count(*) from room_bans`); n != 0 {
		t.Fatalf("dissolve must not ban, bans=%d", n)
	}
	e.assertConsistent(t, room)

	if got, want := e.eventTargets(EventRoomKicked), sorted(m...); !slices.Equal(got, want) {
		t.Fatalf("room.kicked to %v want %v", got, want)
	}
	for _, ev := range e.pub.ofType(EventRoomKicked) {
		if ev.Data["reason"] != "dissolved" {
			t.Fatalf("room.kicked data %v", ev.Data)
		}
	}
	if got := e.eventTargets(EventCodeInvalidated); !slices.Equal(got, []string{owner}) {
		t.Fatalf("code.invalidated to %v want [%s]", got, owner)
	}
	if ev := e.pub.ofType(EventCodeInvalidated)[0]; ev.Data["reason"] != "dissolved" {
		t.Fatalf("code.invalidated data %v", ev.Data)
	}
}

func TestDissolve_FullRoom(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	e.fill(t, owner, 4)
	if s := e.roomState(t, room); s.Status != "full" {
		t.Fatalf("setup: status=%s", s.Status)
	}
	if _, err := e.mgr.Dissolve(bg, owner, room); err != nil {
		t.Fatal(err)
	}
	if s := e.roomState(t, room); s.Status != "open" || s.MemberCount != 1 {
		t.Fatalf("after dissolve: %+v", s)
	}
	e.assertConsistent(t, room)
}

func TestDissolve_Idempotent(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	e.fill(t, owner, 2)
	if _, err := e.mgr.Dissolve(bg, owner, room); err != nil {
		t.Fatal(err)
	}
	e.pub.reset()
	res, err := e.mgr.Dissolve(bg, owner, room)
	if err != nil || res.MemberCount != 1 || res.Status != "open" {
		t.Fatalf("second dissolve res=%+v err=%v", res, err)
	}
	if n := len(e.pub.ofType(EventRoomKicked)); n != 0 {
		t.Fatalf("second dissolve sent %d room.kicked", n)
	}
}

func TestDissolve_Authorization(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	m := e.fill(t, owner, 1)
	stranger := e.user(t, "s@x.test", "18_plus")
	extra := e.extraCode(t, owner)

	cases := []struct {
		name         string
		caller, room string
		want         error
	}{
		{"member is not owner", m[0], room, ErrNotRoomOwner},
		{"stranger", stranger, room, ErrRoomNotFound},
		{"unknown room", owner, "00000000-0000-0000-0000-000000000000", ErrRoomNotFound},
		{"malformed room", owner, "abc", ErrRoomNotFound},
	}
	for _, c := range cases {
		if _, err := e.mgr.Dissolve(bg, c.caller, c.room); !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v want %v", c.name, err, c.want)
		}
	}
	if st := e.codeStatus(t, extra); st != "unused" {
		t.Fatalf("failed dissolve changed a code to %s", st)
	}
	if n := e.memberCount(t, room); n != 2 {
		t.Fatalf("member_count=%d, nothing may change", n)
	}
}

func TestDissolve_MembersCanRejoinOnceWithoutSecondReward(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	oldCode := e.extraCode(t, owner)
	u := e.user(t, "u@x.test", "18_plus")
	e.mustJoin(t, u, oldCode)
	if _, err := e.mgr.Dissolve(bg, owner, room); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.Leave(bg, u, room); err != nil {
		t.Fatalf("a dissolved member leaving again must succeed, got %v", err)
	}
	if _, err := e.join(u, oldCode); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("old code: err=%v want ErrCodeInvalid", err)
	}

	res := e.mustJoin(t, u, e.extraCode(t, owner))
	if len(res.Rewards) != 0 {
		t.Fatalf("rewards on rejoin: %v", res.Rewards)
	}
	if n := count(t, e.pool, `select count(*) from reward_grants where user_id=$1`, u); n != 1 {
		t.Fatalf("reward_grants=%d, want 1", n)
	}
	e.assertConsistent(t, room)
}
