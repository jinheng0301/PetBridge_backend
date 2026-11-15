package service

import (
	"encoding/json"
	"testing"

	"github.com/jinheng0301/petbridge/internal/repository"
)

func repositoryCodes(e *joinEnv) InviteCodeStore { return repository.NewInviteCodeRepo(e.pool) }

func newRoomSvc(e *joinEnv) *RoomService { return NewRoomService(repository.NewRoomRepo(e.pool)) }

func TestRoomMine_OwnRoomFirst(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@x.com", "18_plus")
	b := e.user(t, "b@x.com", "18_plus")
	c := e.user(t, "c@x.com", "18_plus")
	e.mustJoin(t, b, e.signupCode(t, a))
	e.mustJoin(t, c, e.extraCode(t, a))
	aRoom, bRoom := e.roomOf(t, a), e.roomOf(t, b)

	// C leaves A's room: must not appear among its members.
	e.exec(t, `update room_members set left_at=now(), left_reason='left' where room_id=$1 and user_id=$2`, aRoom, c)
	e.exec(t, `update rooms set member_count=member_count-1 where id=$1`, aRoom)

	got, err := newRoomSvc(e).Mine(bg, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}
	if got[0].ID != bRoom || !got[0].IsOwner {
		t.Errorf("first = %s owner=%v, want own room %s owner=true", got[0].ID, got[0].IsOwner, bRoom)
	}
	if got[1].ID != aRoom || got[1].IsOwner {
		t.Errorf("second = %s owner=%v, want joined room %s owner=false", got[1].ID, got[1].IsOwner, aRoom)
	}
	if len(got[0].Members) != 1 || got[0].Members[0].UserID != b {
		t.Errorf("own room members = %+v, want only B", got[0].Members)
	}
	m := got[1].Members
	if len(m) != 2 || m[0].UserID != a || m[1].UserID != b || m[0].SeatNo >= m[1].SeatNo {
		t.Errorf("A's room members = %+v, want [A, B] by seat without C", m)
	}
	if got[1].MemberCount != 2 {
		t.Errorf("member_count = %d, want 2", got[1].MemberCount)
	}
}

func TestRoomMine_EmptyIsNotNil(t *testing.T) {
	e := newJoinEnv(t)
	b := e.bareUser(t, "b@x.com")

	got, err := newRoomSvc(e).Mine(bg, b)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got = %#v, want non-nil empty slice", got)
	}
	if raw, _ := json.Marshal(got); string(raw) != "[]" {
		t.Fatalf("json = %s, want []", raw)
	}
}
