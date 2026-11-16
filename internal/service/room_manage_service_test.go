package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jinheng0301/petbridge/internal/model"
)

// fill adds n new 18_plus users to the owner's room through real joins and
// returns their ids in join order.
func (e *joinEnv) fill(t *testing.T, ownerID string, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		e.seq++
		u := e.user(t, fmt.Sprintf("fill-%d@x.test", e.seq), "18_plus")
		e.mustJoin(t, u, e.extraCode(t, ownerID))
		ids = append(ids, u)
	}
	return ids
}

func (e *joinEnv) roomState(t *testing.T, roomID string) model.RoomState {
	t.Helper()
	var s model.RoomState
	if err := e.pool.QueryRow(bg, `select id::text, owner_id::text, member_count, capacity, status from rooms where id=$1`, roomID).
		Scan(&s.ID, &s.OwnerID, &s.MemberCount, &s.Capacity, &s.Status); err != nil {
		t.Fatal(err)
	}
	return s
}

// assertConsistent checks the invariants every room must keep: member_count is
// the number of active rows, within capacity, and status is full exactly when
// the room is at capacity.
func (e *joinEnv) assertConsistent(t *testing.T, roomID string) {
	t.Helper()
	s := e.roomState(t, roomID)
	active := count(t, e.pool, `select count(*) from room_members where room_id=$1 and left_at is null`, roomID)
	if s.MemberCount != active || s.MemberCount > s.Capacity || s.MemberCount < 1 {
		t.Fatalf("room %s: member_count=%d active rows=%d capacity=%d", roomID, s.MemberCount, active, s.Capacity)
	}
	if (s.Status == "full") != (s.MemberCount == s.Capacity) {
		t.Fatalf("room %s: status=%s with member_count=%d/%d", roomID, s.Status, s.MemberCount, s.Capacity)
	}
}

// eventTargets returns the sorted recipients of every published event of a type.
func (e *joinEnv) eventTargets(typ string) []string {
	var to []string
	for _, ev := range e.pub.ofType(typ) {
		to = append(to, ev.To...)
	}
	slices.Sort(to)
	return to
}

func sorted(ids ...string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

func TestRemove_Success(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	m := e.fill(t, owner, 2)
	e.pub.reset()

	res, err := e.mgr.Remove(bg, owner, room, m[0])
	if err != nil {
		t.Fatal(err)
	}
	if res.MemberCount != 2 || res.ID != room || res.Status != "open" {
		t.Fatalf("summary %+v", res)
	}
	var reason string
	if err := e.pool.QueryRow(bg, `select left_reason from room_members where room_id=$1 and user_id=$2`, room, m[0]).Scan(&reason); err != nil || reason != "kicked" {
		t.Fatalf("left_reason=%q err=%v", reason, err)
	}
	if n := count(t, e.pool, `select count(*) from room_bans where room_id=$1 and user_id=$2 and banned_by=$3`, room, m[0], owner); n != 1 {
		t.Fatalf("ban rows=%d", n)
	}
	e.assertConsistent(t, room)

	if got, want := e.eventTargets(EventMemberLeft), sorted(owner, m[1]); !slices.Equal(got, want) {
		t.Fatalf("member.left to %v want %v", got, want)
	}
	if got := e.eventTargets(EventRoomKicked); !slices.Equal(got, []string{m[0]}) {
		t.Fatalf("room.kicked to %v want [%s]", got, m[0])
	}
	for _, ev := range e.pub.ofType(EventMemberLeft) {
		if ev.Data["reason"] != "kicked" || ev.Data["user_id"] != m[0] {
			t.Fatalf("member.left data %v", ev.Data)
		}
	}
	if ev := e.pub.ofType(EventRoomKicked)[0]; ev.Data["reason"] != "kicked" {
		t.Fatalf("room.kicked data %v", ev.Data)
	}
}

func TestRemove_FullRoomReopens(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	signup := e.signupCode(t, owner)
	m := e.fill(t, owner, 4)
	if s := e.roomState(t, room); s.Status != "full" {
		t.Fatalf("setup: status=%s", s.Status)
	}
	if st := e.codeStatus(t, signup); st != "void" {
		t.Fatalf("setup: signup code %s", st)
	}

	if _, err := e.mgr.Remove(bg, owner, room, m[1]); err != nil {
		t.Fatal(err)
	}
	if s := e.roomState(t, room); s.Status != "open" || s.MemberCount != 4 {
		t.Fatalf("after remove: %+v", s)
	}
	if st := e.codeStatus(t, signup); st != "void" {
		t.Fatalf("voided code must stay void, got %s", st)
	}
	e.assertConsistent(t, room)
}

func TestRemove_Idempotent(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	m := e.fill(t, owner, 2)
	if _, err := e.mgr.Remove(bg, owner, room, m[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.Leave(bg, m[1], room); err != nil {
		t.Fatal(err)
	}
	e.pub.reset()

	for _, target := range []string{m[0], m[1]} { // already kicked; already left
		res, err := e.mgr.Remove(bg, owner, room, target)
		if err != nil || res.MemberCount != 1 {
			t.Fatalf("Remove(%s) res=%+v err=%v", target, res, err)
		}
	}
	if n := len(e.pub.all()); n != 0 {
		t.Fatalf("idempotent removes published %d events", n)
	}
	if n := count(t, e.pool, `select count(*) from room_bans where room_id=$1`, room); n != 1 {
		t.Fatalf("bans=%d, want 1 (a user who left is not banned)", n)
	}
}

func TestRemove_Authorization(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	m := e.fill(t, owner, 2)
	stranger := e.user(t, "s@x.test", "18_plus")

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
		if _, err := e.mgr.Remove(bg, c.caller, c.room, m[1]); !errors.Is(err, c.want) {
			t.Errorf("%s: err=%v want %v", c.name, err, c.want)
		}
	}
	if n := e.memberCount(t, room); n != 3 {
		t.Fatalf("member_count=%d, nothing may change", n)
	}
}

func TestRemove_OwnerTarget(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	if _, err := e.mgr.Remove(bg, owner, room, owner); !errors.Is(err, ErrCannotRemoveOwner) {
		t.Fatalf("err=%v want ErrCannotRemoveOwner", err)
	}
	// A differently-cased spelling of the owner's id is still the owner.
	if _, err := e.mgr.Remove(bg, owner, room, strings.ToUpper(owner)); !errors.Is(err, ErrCannotRemoveOwner) {
		t.Fatalf("upper-case owner id: err=%v want ErrCannotRemoveOwner", err)
	}
	if n := count(t, e.pool, `select count(*) from room_bans`); n != 0 {
		t.Fatalf("bans=%d", n)
	}
	if ok, _ := e.mgr.members.IsActiveMember(bg, room, owner); !ok {
		t.Fatal("owner must stay seated")
	}
	// A malformed target is just "not seated": success with no side effects.
	if res, err := e.mgr.Remove(bg, owner, room, "abc"); err != nil || res.MemberCount != 1 {
		t.Fatalf("Remove(abc) res=%+v err=%v", res, err)
	}
	if n := count(t, e.pool, `select count(*) from room_bans`); n != 0 {
		t.Fatalf("bans=%d", n)
	}
}

func TestRemove_BannedCannotRejoin(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	oldCode := e.extraCode(t, owner)
	u := e.user(t, "kicked@x.test", "18_plus")
	e.mustJoin(t, u, oldCode)
	if _, err := e.mgr.Remove(bg, owner, room, u); err != nil {
		t.Fatal(err)
	}
	if _, err := e.join(u, e.extraCode(t, owner)); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("new code: err=%v want ErrCodeInvalid", err)
	}
	if _, err := e.join(u, oldCode); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("replay of old code: err=%v want ErrCodeInvalid", err)
	}
}

func TestLeave_Success(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	signup := e.signupCode(t, owner)
	m := e.fill(t, owner, 4) // full
	e.pub.reset()

	if err := e.mgr.Leave(bg, m[2], room); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := e.pool.QueryRow(bg, `select left_reason from room_members where room_id=$1 and user_id=$2`, room, m[2]).Scan(&reason); err != nil || reason != "left" {
		t.Fatalf("left_reason=%q err=%v", reason, err)
	}
	if s := e.roomState(t, room); s.Status != "open" || s.MemberCount != 4 {
		t.Fatalf("after leave: %+v", s)
	}
	if st := e.codeStatus(t, signup); st != "void" {
		t.Fatalf("voided code must stay void, got %s", st)
	}
	if n := count(t, e.pool, `select count(*) from room_bans`); n != 0 {
		t.Fatalf("leaving must not ban, bans=%d", n)
	}
	want := sorted(owner, m[0], m[1], m[3])
	if got := e.eventTargets(EventMemberLeft); !slices.Equal(got, want) {
		t.Fatalf("member.left to %v want %v", got, want)
	}
	for _, ev := range e.pub.ofType(EventMemberLeft) {
		if ev.Data["reason"] != "left" || ev.Data["user_id"] != m[2] {
			t.Fatalf("member.left data %v", ev.Data)
		}
	}
	e.assertConsistent(t, room)

	// Rejoining the same room works but pays no second reward.
	res := e.mustJoin(t, m[2], e.extraCode(t, owner))
	if len(res.Rewards) != 0 {
		t.Fatalf("rewards on rejoin: %v", res.Rewards)
	}
	if n := count(t, e.pool, `select count(*) from reward_grants where user_id=$1`, m[2]); n != 1 {
		t.Fatalf("reward_grants=%d, want 1", n)
	}
}

func TestLeave_OwnerRejected(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	if err := e.mgr.Leave(bg, owner, e.roomOf(t, owner)); !errors.Is(err, ErrOwnerCannotLeave) {
		t.Fatalf("err=%v want ErrOwnerCannotLeave", err)
	}
}

func TestLeave_FormerMemberIdempotent(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	m := e.fill(t, owner, 2)
	stranger := e.user(t, "s@x.test", "18_plus")
	if _, err := e.mgr.Remove(bg, owner, room, m[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.Leave(bg, m[1], room); err != nil {
		t.Fatal(err)
	}
	e.pub.reset()

	for name, u := range map[string]string{"kicked": m[0], "left": m[1]} {
		if err := e.mgr.Leave(bg, u, room); err != nil {
			t.Errorf("%s user: err=%v want nil", name, err)
		}
	}
	if n := len(e.pub.all()); n != 0 {
		t.Fatalf("idempotent leave published %d events", n)
	}
	for _, c := range []struct{ user, room string }{
		{stranger, room},
		{owner, "00000000-0000-0000-0000-000000000000"},
		{m[0], "abc"},
	} {
		if err := e.mgr.Leave(bg, c.user, c.room); !errors.Is(err, ErrRoomNotFound) {
			t.Errorf("Leave(%s, %s): err=%v want ErrRoomNotFound", c.user, c.room, err)
		}
	}
}

func TestRemove_EventsSurviveCancel(t *testing.T) {
	e := newJoinEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	m := e.fill(t, owner, 1)
	e.pub.reset()

	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	svc := *e.mgr
	svc.tx = cancelAfterTx{inner: e.txm, cancel: cancel}
	if _, err := svc.Remove(ctx, owner, room, m[0]); err != nil {
		t.Fatal(err)
	}
	if len(e.pub.ofType(EventRoomKicked)) != 1 || len(e.pub.ofType(EventMemberLeft)) != 1 {
		t.Fatalf("events after cancel: %v", e.pub.all())
	}
	if e.pub.deadCalls != 0 {
		t.Fatalf("%d events were published with a cancelled context", e.pub.deadCalls)
	}
}
