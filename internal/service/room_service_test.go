package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/repository"
)

func repositoryCodes(e *joinEnv) InviteCodeStore { return repository.NewInviteCodeRepo(e.pool) }

func newRoomSvc(e *joinEnv) *RoomService {
	return NewRoomService(repository.NewRoomRepo(e.pool), repository.NewUserRepo(e.pool),
		repository.NewInviteCodeRepo(e.pool), 3, func() time.Time { return e.now })
}

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

// The owner's own room carries the regenerations left today in the owner's
// time zone (Asia/Tokyo here); joined rooms carry null (backend TRD 7.8).
func TestRoomMine_RegenRemainingToday(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@x.com", "18_plus")
	b := e.user(t, "b@x.com", "18_plus")
	e.mustJoin(t, b, e.signupCode(t, a))
	bRoom := e.roomOf(t, b)
	seed := func(ago time.Duration, source string) {
		t.Helper()
		code, _ := NewCode()
		e.exec(t, `insert into invite_codes(code, room_id, issued_to, source, status, issued_at, expires_at)
			values($1,$2,$3,$4,'void',$5,$5::timestamptz + interval '7 days')`, code, bRoom, b, source, e.now.Add(-ago))
	}
	remaining := func() (own, other *int) {
		t.Helper()
		got, err := newRoomSvc(e).Mine(bg, b)
		if err != nil || len(got) != 2 {
			t.Fatalf("mine = %+v, %v", got, err)
		}
		return got[0].RegenRemainingToday, got[1].RegenRemainingToday
	}

	own, other := remaining()
	if own == nil || *own != 3 || other != nil {
		t.Fatalf("fresh: own=%v other=%v, want 3 and nil", own, other)
	}

	// now is 21:00 in Tokyo. 20h ago is 01:00 Tokyo today (yesterday in UTC);
	// 22h ago is 23:00 Tokyo yesterday. Other sources do not count.
	seed(time.Hour, "owner_regen")
	seed(20*time.Hour, "owner_regen")
	seed(22*time.Hour, "owner_regen")
	seed(time.Hour, "join_reward")
	if own, _ := remaining(); own == nil || *own != 1 {
		t.Fatalf("after 2 today: own=%v, want 1", own)
	}

	seed(2*time.Hour, "owner_regen")
	seed(3*time.Hour, "owner_regen")
	if own, _ := remaining(); own == nil || *own != 0 {
		t.Fatalf("over the limit: own=%v, want 0", own)
	}

	got, _ := newRoomSvc(e).Mine(bg, b)
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"regen_remaining_today":0`) || !strings.Contains(string(raw), `"regen_remaining_today":null`) {
		t.Fatalf("json = %s, want 0 for own room and null for the joined one", raw)
	}
}
