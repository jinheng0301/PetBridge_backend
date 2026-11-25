package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

// snapshotJSON returns the snapshot as the client sees it.
func snapshotJSON(t *testing.T, svc *RealtimeService, user, room string) (body map[string]any, members map[string]map[string]any) {
	t.Helper()
	snap, err := svc.Snapshot(bg, user, room)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	body = out["room"].(map[string]any)
	members = map[string]map[string]any{}
	for _, m := range body["members"].([]any) {
		mm := m.(map[string]any)
		members[mm["user_id"].(string)] = mm
	}
	return body, members
}

func assertProgress(t *testing.T, body map[string]any, total, next float64) {
	t.Helper()
	p := body["progress"].(map[string]any)
	if p["energy_total"] != total || p["next_threshold"] != next {
		t.Fatalf("progress %v want %v/%v", p, total, next)
	}
}

func TestSnapshot_CheckinFields(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	store := repository.NewCheckinRepo(e.pool)
	at := mustUTC(t, "2026-11-15T12:00:00Z")
	clock := func() time.Time { return at }

	for name, svc := range map[string]*RealtimeService{
		"without a store": newRealtimeSvc(e),
		"fresh room":      newRealtimeSvc(e).WithCheckins(store, clock),
	} {
		body, members := snapshotJSON(t, svc, owner, room)
		assertProgress(t, body, 0, 10)
		if d, ok := body["decor"].([]any); !ok || len(d) != 0 {
			t.Fatalf("%s: decor %v", name, body["decor"])
		}
		for id, m := range members {
			if v, ok := m["checkin_expires_at"]; !ok || v != nil {
				t.Fatalf("%s: member %s checkin_expires_at %v present=%v", name, id, v, ok)
			}
		}
	}

	svc := newRealtimeSvc(e).WithCheckins(store, clock)
	checkins := NewCheckinService(e.txm, repository.NewUserRepo(e.pool), repository.NewRoomRepo(e.pool),
		repository.NewRoomMemberRepo(e.pool), store, e.joinEnv.pub, clock)
	if _, err := checkins.Checkin(bg, owner, room); err != nil {
		t.Fatal(err)
	}
	body, members := snapshotJSON(t, svc, member, strings.ToUpper(room))
	assertProgress(t, body, 10, 40)
	d := body["decor"].([]any)
	if len(d) != 1 || d[0].(map[string]any)["item_id"] != "wall_clock" || d[0].(map[string]any)["unlocked_at"] != "2026-11-15T12:00:00Z" {
		t.Fatalf("decor %v", d)
	}
	if members[owner]["checkin_expires_at"] != "2026-11-15T15:00:00Z" || members[member]["checkin_expires_at"] != nil {
		t.Fatalf("members %v", members)
	}
	at = mustUTC(t, "2026-11-15T15:00:00Z") // Tokyo midnight
	if _, members = snapshotJSON(t, svc, member, room); members[owner]["checkin_expires_at"] != nil {
		t.Fatalf("after midnight %v", members[owner])
	}
}

func TestSnapshot_UnknownDecorKept(t *testing.T) {
	e := newRTEnv(t)
	owner, _, room := e.twoInRoom(t)
	store := repository.NewCheckinRepo(e.pool)
	if _, err := store.UnlockDecor(bg, room, []string{"old_item"}, e.now); err != nil {
		t.Fatal(err)
	}
	body, _ := snapshotJSON(t, newRealtimeSvc(e).WithCheckins(store, time.Now), owner, room)
	if d := body["decor"].([]any); len(d) != 1 || d[0].(map[string]any)["item_id"] != "old_item" {
		t.Fatalf("decor %v", d)
	}
}

var errStoreDown = errors.New("store down")

// brokenCheckins fails one of the reads a snapshot makes.
type brokenCheckins struct {
	CheckinStore
	fail string
}

func (b brokenCheckins) Energy(ctx context.Context, room string) (int, error) {
	if b.fail == "energy" {
		return 0, errStoreDown
	}
	return b.CheckinStore.Energy(ctx, room)
}

func (b brokenCheckins) ListDecor(ctx context.Context, room string) ([]model.DecorUnlock, error) {
	if b.fail == "decor" {
		return nil, errStoreDown
	}
	return b.CheckinStore.ListDecor(ctx, room)
}

func (b brokenCheckins) MemberCheckins(ctx context.Context, room string) ([]model.MemberCheckin, error) {
	if b.fail == "members" {
		return nil, errStoreDown
	}
	return b.CheckinStore.MemberCheckins(ctx, room)
}

func TestSnapshot_StoreErrorFails(t *testing.T) {
	e := newRTEnv(t)
	owner, _, room := e.twoInRoom(t)
	for _, f := range []string{"energy", "decor", "members"} {
		svc := newRealtimeSvc(e).WithCheckins(brokenCheckins{repository.NewCheckinRepo(e.pool), f}, time.Now)
		if _, err := svc.Snapshot(bg, owner, room); !errors.Is(err, errStoreDown) {
			t.Fatalf("%s: err=%v", f, err)
		}
	}
}

// member.joined keeps its B4 shape; check-in state travels in snapshots only.
func TestMemberJoined_HasNoCheckinField(t *testing.T) {
	e := newRTEnv(t)
	owner, _, room := e.twoInRoom(t)
	watcher := e.connect(t, "cw", owner, room)
	third := e.user(t, "t@x.test", "18_plus")
	e.pub.Publish(bg, Event{Type: EventMemberJoined, Kind: KindState, RoomID: room, To: []string{owner},
		Data: map[string]any{"room_id": room, "member": model.MemberInfo{UserID: third, SeatNo: 3, Role: "member"}}})
	f := framesOf(watcher, EventMemberJoined)
	if len(f) != 1 {
		t.Fatalf("frames %d", len(f))
	}
	b, err := json.Marshal(f[0])
	if err != nil || strings.Contains(string(b), "checkin") || !strings.Contains(string(b), third) {
		t.Fatalf("frame %s err=%v", b, err)
	}
}
