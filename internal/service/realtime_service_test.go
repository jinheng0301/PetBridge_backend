package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/repository"
)

// callLog records the order of seq and member reads.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(s string) { l.mu.Lock(); l.calls = append(l.calls, s); l.mu.Unlock() }

type loggingSeqs struct{ log *callLog }

func (s loggingSeqs) CurrentSeq(context.Context, string) int64 { s.log.add("seq"); return 9 }

type loggingRooms struct {
	RoomStore
	log *callLog
}

func (r loggingRooms) ListForMember(ctx context.Context, userID string) ([]model.RoomDetail, error) {
	r.log.add("members")
	return r.RoomStore.ListForMember(ctx, userID)
}

func newRealtimeSvc(e *rtEnv) *RealtimeService {
	return NewRealtimeService(repository.NewRoomRepo(e.pool), repository.NewRoomMemberRepo(e.pool), e.presence, e.pub, e.joinEnv.pub)
}

func TestSnapshot_Member(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	e.connect(t, "co", owner)
	svc := newRealtimeSvc(e)

	snap, err := svc.Snapshot(bg, owner, room)
	if err != nil {
		t.Fatal(err)
	}
	if snap.RoomID != room || snap.Room.ID != room || !snap.Room.IsOwner || snap.Room.MemberCount != 2 {
		t.Fatalf("owner snapshot %+v", snap)
	}
	ms := snap.Room.Members
	if len(ms) != 2 || ms[0].UserID != owner || ms[0].SeatNo > ms[1].SeatNo {
		t.Fatalf("members %+v", ms)
	}
	if ms[0].Presence != model.PresenceOnline || ms[1].Presence != model.PresenceOffline {
		t.Fatalf("presence %s/%s", ms[0].Presence, ms[1].Presence)
	}
	snap, err = svc.Snapshot(bg, member, strings.ToUpper(room))
	if err != nil || snap.Room.IsOwner || snap.RoomID != room {
		t.Fatalf("member snapshot %+v err=%v", snap, err)
	}
}

func TestSnapshot_NotMember(t *testing.T) {
	e := newRTEnv(t)
	owner, _, room := e.twoInRoom(t)
	stranger := e.user(t, "s@x.test", "18_plus")
	svc := newRealtimeSvc(e)
	cases := []struct{ user, room string }{
		{stranger, room},
		{owner, "00000000-0000-0000-0000-000000000000"},
		{owner, "abc"},
	}
	for _, c := range cases {
		if _, err := svc.Snapshot(bg, c.user, c.room); !errors.Is(err, ErrRoomNotFound) {
			t.Fatalf("Snapshot(%s, %q) err=%v want ErrRoomNotFound", c.user, c.room, err)
		}
	}
}

func TestSnapshot_SeqReadBeforeMembers(t *testing.T) {
	e := newRTEnv(t)
	owner, _, room := e.twoInRoom(t)
	log := &callLog{}
	svc := NewRealtimeService(loggingRooms{repository.NewRoomRepo(e.pool), log},
		repository.NewRoomMemberRepo(e.pool), e.presence, loggingSeqs{log}, e.joinEnv.pub)
	snap, err := svc.Snapshot(bg, owner, room)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(log.calls, []string{"seq", "members"}) || snap.Seq != 9 {
		t.Fatalf("calls %v seq %d", log.calls, snap.Seq)
	}
}

func TestPetTap_Relays(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	third := e.user(t, "t@x.test", "18_plus")
	e.mustJoin(t, third, e.extraCode(t, owner))
	e.joinEnv.pub.reset()

	if err := newRealtimeSvc(e).PetTap(bg, owner, room, member); err != nil {
		t.Fatal(err)
	}
	evs := e.joinEnv.pub.ofType(EventPetAction)
	if len(evs) != 1 {
		t.Fatalf("pet.action events %d", len(evs))
	}
	ev := evs[0]
	if ev.Kind != KindTransient || !slices.Equal(sorted(ev.To...), sorted(member, third)) {
		t.Fatalf("pet.action %+v", ev)
	}
	want := map[string]any{"room_id": room, "from_user_id": owner, "target_user_id": member, "action": "tap"}
	for k, v := range want {
		if ev.Data[k] != v {
			t.Fatalf("data %v want %v", ev.Data, want)
		}
	}
}

func TestPetTap_Rejects(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	stranger := e.user(t, "s@x.test", "18_plus")
	svc := newRealtimeSvc(e)
	e.joinEnv.pub.reset()
	for _, c := range []struct{ from, room, target string }{
		{owner, room, owner},    // self
		{owner, room, stranger}, // target not a member
		{owner, "abc", member},  // malformed room id
	} {
		if err := svc.PetTap(bg, c.from, c.room, c.target); !errors.Is(err, ErrRoomNotFound) {
			t.Fatalf("PetTap(%+v) err=%v", c, err)
		}
	}
	if err := e.mgr.Leave(bg, member, room); err != nil {
		t.Fatal(err)
	}
	e.joinEnv.pub.reset()
	if err := svc.PetTap(bg, member, room, owner); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("former member tap err=%v", err)
	}
	if n := len(e.joinEnv.pub.ofType(EventPetAction)); n != 0 {
		t.Fatalf("rejected taps published %d events", n)
	}
}
