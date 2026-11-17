package service

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jinheng0301/petbridge/internal/model"
)

// fakeConn records what the hub sends it.
type fakeConn struct {
	id, user string

	mu        sync.Mutex
	frames    []model.Frame
	full      bool // Send reports a full queue
	closed    bool
	closeCode int
}

func newFakeConn(id, user string) *fakeConn { return &fakeConn{id: id, user: user} }

func (c *fakeConn) ID() string     { return c.id }
func (c *fakeConn) UserID() string { return c.user }

func (c *fakeConn) Send(f model.Frame) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.full {
		return false
	}
	c.frames = append(c.frames, f)
	return true
}

func (c *fakeConn) Close(code int, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed, c.closeCode = true, code
}

func (c *fakeConn) got() []model.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]model.Frame(nil), c.frames...)
}

func (c *fakeConn) types() []string {
	var out []string
	for _, f := range c.got() {
		out = append(out, f.T)
	}
	return out
}

func (c *fakeConn) closedWith() (bool, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed, c.closeCode
}

func testHub() *Hub { return NewHub(DefaultRealtimeConfig("0.0.0")) }

// subscribe runs a full BeginSub/EndSub with an empty snapshot at seq 0.
func subscribe(t *testing.T, h *Hub, c Conn, room string) {
	t.Helper()
	if err := h.BeginSub(c, room); err != nil {
		t.Fatal(err)
	}
	h.EndSub(c, room, model.RoomSnapshot{RoomID: room})
}

func stateEvent(room string, to ...string) Event {
	return Event{Type: EventMemberJoined, Kind: KindState, RoomID: room, To: to, Data: map[string]any{"room_id": room}}
}

func TestHub_StateEventOnlyToSubscribers(t *testing.T) {
	h := testHub()
	a, b := newFakeConn("ca", "A"), newFakeConn("cb", "B")
	h.Register(a)
	h.Register(b)
	subscribe(t, h, a, "r1")

	h.Deliver(stateEvent("r1", "A", "B"), 7)

	got := a.got()
	if last := got[len(got)-1]; last.T != EventMemberJoined || last.Seq != 7 {
		t.Fatalf("A got %+v", got)
	}
	if len(b.got()) != 0 {
		t.Fatalf("unsubscribed B got %+v", b.got())
	}
}

func TestHub_StateEventSkipsNonRecipients(t *testing.T) {
	h := testHub()
	a := newFakeConn("ca", "A")
	h.Register(a)
	subscribe(t, h, a, "r1")
	h.Deliver(stateEvent("r1", "B"), 1)
	if types := a.types(); len(types) != 1 { // only the snapshot
		t.Fatalf("A got %v", types)
	}
}

func TestHub_PersonalEventIgnoresSubscription(t *testing.T) {
	h := testHub()
	a := newFakeConn("ca", "A")
	h.Register(a)
	h.Deliver(Event{Type: EventCodeInvalidated, Kind: KindPersonal, RoomID: "r1", To: []string{"A"}}, 0)
	if got := a.got(); len(got) != 1 || got[0].T != EventCodeInvalidated || got[0].Seq != 0 {
		t.Fatalf("A got %+v", got)
	}
}

func TestHub_KickedUnsubscribes(t *testing.T) {
	h := testHub()
	a := newFakeConn("ca", "A")
	h.Register(a)
	subscribe(t, h, a, "r1")

	h.Deliver(roomKickedEvents("r1", "kicked", []string{"A"})[0], 0)
	if h.Subscribed(a, "r1") {
		t.Fatal("room.kicked must end the subscription")
	}
	h.Deliver(memberLeftEvent("r1", "X", "left", []string{"A"}), 3)
	want := []string{model.FrameRoomSnapshot, EventRoomKicked}
	if got := a.types(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("A got %v want %v", got, want)
	}
}

func TestHub_FourthConnEvictsOldest(t *testing.T) {
	h := testHub()
	conns := []*fakeConn{newFakeConn("c1", "A"), newFakeConn("c2", "A"), newFakeConn("c3", "A"), newFakeConn("c4", "A")}
	for _, c := range conns {
		h.Register(c)
	}
	if closed, code := conns[0].closedWith(); !closed || code != model.CloseReplaced {
		t.Fatalf("oldest conn closed=%v code=%d", closed, code)
	}
	for _, c := range conns[1:] {
		if closed, _ := c.closedWith(); closed {
			t.Fatalf("conn %s must stay open", c.id)
		}
	}
	if n := h.LocalCount("A"); n != 3 {
		t.Fatalf("LocalCount=%d want 3", n)
	}
}

func TestHub_SlowConsumerClosed(t *testing.T) {
	h := testHub()
	slow, ok := newFakeConn("cs", "A"), newFakeConn("co", "B")
	h.Register(slow)
	h.Register(ok)
	subscribe(t, h, slow, "r1")
	subscribe(t, h, ok, "r1")
	slow.mu.Lock()
	slow.full = true
	slow.mu.Unlock()

	h.Deliver(stateEvent("r1", "A", "B"), 1)

	if closed, code := slow.closedWith(); !closed || code != model.ClosePolicy {
		t.Fatalf("slow conn closed=%v code=%d", closed, code)
	}
	if h.LocalCount("A") != 0 {
		t.Fatal("slow conn must be unregistered")
	}
	if types := ok.types(); len(types) != 2 || types[1] != EventMemberJoined {
		t.Fatalf("B got %v", types)
	}
}

func TestHub_SnapshotThenBuffered(t *testing.T) {
	h := testHub()
	a := newFakeConn("ca", "A")
	h.Register(a)
	if err := h.BeginSub(a, "r1"); err != nil {
		t.Fatal(err)
	}
	h.Deliver(stateEvent("r1", "A"), 3)
	h.Deliver(stateEvent("r1", "A"), 5)
	h.Deliver(Event{Type: EventPresenceUpdate, Kind: KindTransient, RoomID: "r1", To: []string{"A"}}, 0)
	if len(a.got()) != 0 {
		t.Fatalf("nothing may be sent before the snapshot, got %v", a.types())
	}

	h.EndSub(a, "r1", model.RoomSnapshot{RoomID: "r1", Seq: 4})

	got := a.got()
	if len(got) != 3 || got[0].T != model.FrameRoomSnapshot || got[1].Seq != 5 || got[2].T != EventPresenceUpdate {
		t.Fatalf("got %+v", got)
	}
	h.Deliver(stateEvent("r1", "A"), 6)
	if got := a.got(); got[len(got)-1].Seq != 6 {
		t.Fatalf("after EndSub events flow directly, got %+v", got)
	}
}

func TestHub_CancelSubDropsBuffer(t *testing.T) {
	h := testHub()
	a := newFakeConn("ca", "A")
	h.Register(a)
	_ = h.BeginSub(a, "r1")
	h.Deliver(stateEvent("r1", "A"), 1)
	h.CancelSub(a, "r1")
	h.Deliver(stateEvent("r1", "A"), 2)
	if len(a.got()) != 0 || h.Subscribed(a, "r1") {
		t.Fatalf("cancelled sub got %v", a.types())
	}
}

func TestHub_SubLimit(t *testing.T) {
	h := testHub()
	a := newFakeConn("ca", "A")
	h.Register(a)
	subscribe(t, h, a, "r1")
	subscribe(t, h, a, "r2")
	if err := h.BeginSub(a, "r3"); !errors.Is(err, ErrTooManySubs) {
		t.Fatalf("third room err=%v want ErrTooManySubs", err)
	}
	if err := h.BeginSub(a, "r1"); err != nil {
		t.Fatalf("re-subscribing a room already held must work, got %v", err)
	}
	h.Unsub(a, "r2")
	if err := h.BeginSub(a, "r3"); err != nil {
		t.Fatalf("after unsub err=%v", err)
	}
}

func TestHub_BroadcastAndShutdown(t *testing.T) {
	h := testHub()
	a, b := newFakeConn("ca", "A"), newFakeConn("cb", "B")
	h.Register(a)
	h.Register(b)
	h.Broadcast(model.Frame{T: model.FrameResync})
	h.Shutdown()
	for _, c := range []*fakeConn{a, b} {
		want := []string{model.FrameResync, model.FrameServerRestart}
		if got := c.types(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s got %v want %v", c.id, got, want)
		}
		if closed, code := c.closedWith(); !closed || code != model.CloseGoingAway {
			t.Fatalf("%s closed=%v code=%d", c.id, closed, code)
		}
	}
}

func TestHub_ConcurrentDeliverAndRegister(t *testing.T) {
	h := testHub()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := newFakeConn(fmt.Sprintf("c%d", i), fmt.Sprintf("U%d", i%7))
			h.Register(c)
			if h.BeginSub(c, "r1") == nil {
				h.EndSub(c, "r1", model.RoomSnapshot{RoomID: "r1"})
			}
			h.Deliver(stateEvent("r1", "U1", "U2", "U3"), int64(i+1))
			h.Unregister(c)
		}(i)
	}
	wg.Wait()
}

func TestHub_RegisterAfterShutdownCloses(t *testing.T) {
	h := testHub()
	h.Shutdown()
	late := newFakeConn("cl", "A")
	h.Register(late)
	if got := late.types(); len(got) != 1 || got[0] != model.FrameServerRestart {
		t.Fatalf("late conn got %v", got)
	}
	if closed, code := late.closedWith(); !closed || code != model.CloseGoingAway {
		t.Fatalf("late conn closed=%v code=%d", closed, code)
	}
	if h.LocalCount("A") != 0 {
		t.Fatal("late conn must not be registered")
	}
}

// A subscription stuck before its snapshot must not buffer without bound.
func TestHub_PendingBufferOverflowCloses(t *testing.T) {
	cfg := DefaultRealtimeConfig("0.0.0")
	cfg.SendQueue = 4
	h := NewHub(cfg)
	a := newFakeConn("ca", "A")
	h.Register(a)
	if err := h.BeginSub(a, "r1"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		h.Deliver(stateEvent("r1", "A"), int64(i))
	}
	if closed, _ := a.closedWith(); closed {
		t.Fatal("closed before the buffer was full")
	}
	h.Deliver(stateEvent("r1", "A"), 5)
	if closed, code := a.closedWith(); !closed || code != model.ClosePolicy {
		t.Fatalf("closed=%v code=%d", closed, code)
	}
	if h.LocalCount("A") != 0 {
		t.Fatal("overflowing conn must be unregistered")
	}
}
