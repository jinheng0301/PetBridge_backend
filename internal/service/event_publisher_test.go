package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/repository"
)

var errBusDown = errors.New("bus down")

// fakeBus is an in-process Bus: Publish hands the payload straight to the
// handler registered by Run. fail makes every call error, as a dead Redis would.
type fakeBus struct {
	mu        sync.Mutex
	fail      bool
	seq       map[string]int64
	onMsg     func([]byte)
	onRe      func()
	ready     chan struct{}
	readyOnce sync.Once
}

func newFakeBus() *fakeBus { return &fakeBus{seq: map[string]int64{}, ready: make(chan struct{})} }

func (b *fakeBus) setFail(v bool) { b.mu.Lock(); b.fail = v; b.mu.Unlock() }

func (b *fakeBus) NextSeq(ctx context.Context, roomID string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if b.fail {
		return 0, errBusDown
	}
	b.seq[roomID]++
	return b.seq[roomID], nil
}

func (b *fakeBus) CurrentSeq(_ context.Context, roomID string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return 0, errBusDown
	}
	return b.seq[roomID], nil
}

func (b *fakeBus) Publish(ctx context.Context, payload []byte) error {
	b.mu.Lock()
	fail, h := b.fail, b.onMsg
	b.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if fail {
		return errBusDown
	}
	if h != nil {
		h(payload)
	}
	return nil
}

func (b *fakeBus) Run(ctx context.Context, onMessage func([]byte), onReconnect func()) {
	b.mu.Lock()
	b.onMsg, b.onRe = onMessage, onReconnect
	b.mu.Unlock()
	b.readyOnce.Do(func() { close(b.ready) })
	<-ctx.Done()
}

func (b *fakeBus) reconnect() {
	b.mu.Lock()
	h := b.onRe
	b.mu.Unlock()
	h()
}

// memPresence is an in-process PresenceStore; fail makes every call error.
type memPresence struct {
	mu        sync.Mutex
	fail      bool
	conns     map[string]map[string]time.Time
	ttl       time.Duration
	beforeAdd func(connID string) // runs before Add applies, outside the lock
}

func newMemPresence() *memPresence {
	return &memPresence{conns: map[string]map[string]time.Time{}, ttl: 70 * time.Second}
}

func (p *memPresence) Add(_ context.Context, u, c string, now time.Time) (int, error) {
	p.mu.Lock()
	hook := p.beforeAdd
	p.mu.Unlock()
	if hook != nil {
		hook(c)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return 0, errBusDown
	}
	if p.conns[u] == nil {
		p.conns[u] = map[string]time.Time{}
	}
	p.conns[u][c] = now.Add(p.ttl)
	return p.countLocked(u, now), nil
}

func (p *memPresence) Remove(_ context.Context, u, c string, now time.Time) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return 0, errBusDown
	}
	delete(p.conns[u], c)
	return p.countLocked(u, now), nil
}

func (p *memPresence) Count(_ context.Context, u string, now time.Time) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return 0, errBusDown
	}
	return p.countLocked(u, now), nil
}

// Sweep implements PresenceSweeper like the Redis store: it prunes expired
// members and reports users it emptied.
func (p *memPresence) Sweep(_ context.Context, now time.Time) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return nil, errBusDown
	}
	var out []string
	for u, conns := range p.conns {
		removed := 0
		for c, exp := range conns {
			if !exp.After(now) {
				delete(conns, c)
				removed++
			}
		}
		if removed > 0 && len(conns) == 0 {
			out = append(out, u)
		}
	}
	return out, nil
}

func (p *memPresence) countLocked(u string, now time.Time) int {
	n := 0
	for _, exp := range p.conns[u] {
		if exp.After(now) {
			n++
		}
	}
	return n
}

// rtEnv wires the realtime pieces over a joinEnv whose services publish
// through the real EventPublisher.
type rtEnv struct {
	*joinEnv
	bus       *fakeBus
	store     *memPresence
	hub       *Hub
	pub       *EventPublisher
	presence  *PresenceService
	fallbacks int
	mu        sync.Mutex
}

func newRTEnv(t *testing.T) *rtEnv {
	t.Helper()
	e := &rtEnv{joinEnv: newJoinEnv(t), bus: newFakeBus(), store: newMemPresence()}
	cfg := DefaultRealtimeConfig("0.0.0")
	e.hub = NewHub(cfg)
	e.pub = NewEventPublisher(e.bus, e.hub, func(error) { e.mu.Lock(); e.fallbacks++; e.mu.Unlock() })
	e.presence = NewPresenceService(e.store, e.hub, repository.NewRoomRepo(e.pool), e.pub, cfg, time.Now)
	e.pub.SetPresence(e.presence)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.pub.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	<-e.bus.ready
	return e
}

// connect registers a live, presence-tracked connection for user and
// subscribes it to rooms.
func (e *rtEnv) connect(t *testing.T, id, user string, rooms ...string) *fakeConn {
	t.Helper()
	c := newFakeConn(id, user)
	e.hub.Register(c)
	e.presence.Connected(bg, user, id)
	for _, r := range rooms {
		subscribe(t, e.hub, c, r)
	}
	return c
}

func (e *rtEnv) disconnect(c *fakeConn) {
	e.hub.Unregister(c)
	e.presence.Disconnected(bg, c.user, c.id)
}

func framesOf(c *fakeConn, typ string) []model.Frame {
	var out []model.Frame
	for _, f := range c.got() {
		if f.T == typ {
			out = append(out, f)
		}
	}
	return out
}

func presenceOf(t *testing.T, f model.Frame) (user, status string) {
	t.Helper()
	d, ok := f.D.(map[string]any)
	if !ok {
		t.Fatalf("presence.update d=%T", f.D)
	}
	return d["user_id"].(string), d["status"].(string)
}

func TestPublisher_StateEventGetsSeq(t *testing.T) {
	e := newRTEnv(t)
	a := e.connect(t, "ca", "A", "r1")
	e.pub.Publish(bg, memberLeftEvent("r1", "X", "left", []string{"A"}))
	e.pub.Publish(bg, memberLeftEvent("r1", "Y", "left", []string{"A"}))
	got := framesOf(a, EventMemberLeft)
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("member.left frames %+v", got)
	}
	if n := e.pub.CurrentSeq(bg, "r1"); n != 2 {
		t.Fatalf("CurrentSeq=%d want 2", n)
	}
}

func TestPublisher_TransientAndPersonalHaveNoSeq(t *testing.T) {
	e := newRTEnv(t)
	a := e.connect(t, "ca", "A", "r1")
	e.pub.Publish(bg, Event{Type: EventPetAction, Kind: KindTransient, RoomID: "r1", To: []string{"A"}, Data: map[string]any{"room_id": "r1"}})
	e.pub.Publish(bg, roomKickedEvents("r1", "kicked", []string{"A"})[0])
	for _, typ := range []string{EventPetAction, EventRoomKicked} {
		f := framesOf(a, typ)
		if len(f) != 1 || f[0].Seq != 0 {
			t.Fatalf("%s frames %+v", typ, f)
		}
	}
	if n := e.pub.CurrentSeq(bg, "r1"); n != 0 {
		t.Fatalf("seq advanced to %d by seq-less events", n)
	}
}

func TestPublisher_FallbackDeliversWithoutSeq(t *testing.T) {
	e := newRTEnv(t)
	a := e.connect(t, "ca", "A", "r1")
	e.bus.setFail(true)
	e.pub.Publish(bg, stateEvent("r1", "A"))
	f := framesOf(a, EventMemberJoined)
	if len(f) != 1 || f[0].Seq != 0 {
		t.Fatalf("fallback frames %+v", f)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fallbacks != 1 {
		t.Fatalf("onFallback calls=%d want 1", e.fallbacks)
	}
}

func TestPublisher_JoinedCarriesPresence(t *testing.T) {
	e := newRTEnv(t)
	owner := e.user(t, "o@x.test", "18_plus")
	room := e.roomOf(t, owner)
	a := e.connect(t, "co", owner, room)

	for _, tc := range []struct {
		email  string
		online bool
		want   string
	}{{"on@x.test", true, model.PresenceOnline}, {"off@x.test", false, model.PresenceOffline}} {
		u := e.user(t, tc.email, "18_plus")
		if tc.online {
			e.connect(t, "c-"+u, u)
		}
		res := e.mustJoin(t, u, e.extraCode(t, owner))
		for _, ev := range joinEvents(room, u, res.Members, nil) {
			e.pub.Publish(bg, ev)
		}
		f := framesOf(a, EventMemberJoined)
		d := f[len(f)-1].D.(map[string]any)
		member := d["member"].(map[string]any)
		if member["user_id"] != u || member["presence"] != tc.want {
			t.Fatalf("%s: member %v want presence %s", tc.email, member, tc.want)
		}
	}
}

func TestPublisher_ResyncOnReconnect(t *testing.T) {
	e := newRTEnv(t)
	a := e.connect(t, "ca", "A")
	b := e.connect(t, "cb", "B", "r1")
	e.bus.reconnect()
	for _, c := range []*fakeConn{a, b} {
		if f := framesOf(c, model.FrameResync); len(f) != 1 {
			t.Fatalf("%s resync frames %d", c.id, len(f))
		}
	}
}

// twoInRoom returns an owner and a member of the owner's room and the room id.
func (e *rtEnv) twoInRoom(t *testing.T) (owner, member, room string) {
	t.Helper()
	owner = e.user(t, "o@x.test", "18_plus")
	room = e.roomOf(t, owner)
	member = e.user(t, "m@x.test", "18_plus")
	e.mustJoin(t, member, e.extraCode(t, owner))
	return owner, member, room
}

func TestPresence_FirstConnBroadcastsOnline(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	watcher := e.connect(t, "cw", owner, room)
	e.connect(t, "cm", member)
	f := framesOf(watcher, EventPresenceUpdate)
	if len(f) != 1 {
		t.Fatalf("presence frames %d want 1", len(f))
	}
	if u, s := presenceOf(t, f[0]); u != member || s != model.PresenceOnline || f[0].Seq != 0 {
		t.Fatalf("presence %s %s seq %d", u, s, f[0].Seq)
	}
	if got := e.presence.Status(bg, member); got != model.PresenceOnline {
		t.Fatalf("Status=%s", got)
	}
}

func TestPresence_SecondConnKeepsOnline(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	watcher := e.connect(t, "cw", owner, room)
	c1 := e.connect(t, "c1", member)
	c2 := e.connect(t, "c2", member)
	if n := len(framesOf(watcher, EventPresenceUpdate)); n != 1 {
		t.Fatalf("second connection must not re-announce online, frames=%d", n)
	}
	e.disconnect(c1)
	if n := len(framesOf(watcher, EventPresenceUpdate)); n != 1 {
		t.Fatalf("closing one of two connections must not go offline, frames=%d", n)
	}
	e.disconnect(c2)
	f := framesOf(watcher, EventPresenceUpdate)
	if len(f) != 2 {
		t.Fatalf("frames=%d want 2", len(f))
	}
	if _, s := presenceOf(t, f[1]); s != model.PresenceOffline {
		t.Fatalf("last close status=%s", s)
	}
}

func TestPresence_ReturningUserWakes(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	watcher := e.connect(t, "cw", owner, room)
	e.disconnect(e.connect(t, "c1", member))
	e.connect(t, "c2", member)
	var statuses []string
	for _, f := range framesOf(watcher, EventPresenceUpdate) {
		_, s := presenceOf(t, f)
		statuses = append(statuses, s)
	}
	want := []string{model.PresenceOnline, model.PresenceOffline, model.PresenceOnline}
	if len(statuses) != 3 || statuses[0] != want[0] || statuses[1] != want[1] || statuses[2] != want[2] {
		t.Fatalf("statuses %v want %v", statuses, want)
	}
}

func TestPresence_NotSentToSelf(t *testing.T) {
	e := newRTEnv(t)
	owner, _, room := e.twoInRoom(t)
	self := e.connect(t, "co", owner, room)
	if n := len(framesOf(self, EventPresenceUpdate)); n != 0 {
		t.Fatalf("own presence echoed %d times", n)
	}
}

func TestPresence_StoreDownUsesLocalCount(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	watcher := e.connect(t, "cw", owner, room)
	e.store.mu.Lock()
	e.store.fail = true
	e.store.mu.Unlock()

	c1 := e.connect(t, "c1", member)
	if got := e.presence.Status(bg, member); got != model.PresenceOnline {
		t.Fatalf("Status with store down=%s", got)
	}
	e.disconnect(c1)
	var statuses []string
	for _, f := range framesOf(watcher, EventPresenceUpdate) {
		_, s := presenceOf(t, f)
		statuses = append(statuses, s)
	}
	if len(statuses) != 2 || statuses[0] != model.PresenceOnline || statuses[1] != model.PresenceOffline {
		t.Fatalf("statuses %v", statuses)
	}
	if got := e.presence.Status(bg, member); got != model.PresenceOffline {
		t.Fatalf("Status after close=%s", got)
	}
}

// One device closes while another connects: whatever the interleaving, the
// last word roommates hear must be online.
func TestPresence_CloseDuringConnectStaysOnline(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	watcher := e.connect(t, "cw", owner, room)
	c1 := e.connect(t, "c1", member)

	c2 := newFakeConn("c2", member)
	e.hub.Register(c2)
	e.store.mu.Lock()
	e.store.beforeAdd = func(conn string) {
		if conn == "c2" {
			e.store.mu.Lock()
			e.store.beforeAdd = nil
			e.store.mu.Unlock()
			e.disconnect(c1) // c1 closes between c2's checks
		}
	}
	e.store.mu.Unlock()
	e.presence.Connected(bg, member, "c2")

	f := framesOf(watcher, EventPresenceUpdate)
	if _, s := presenceOf(t, f[len(f)-1]); s != model.PresenceOnline {
		var all []string
		for _, x := range f {
			_, st := presenceOf(t, x)
			all = append(all, st)
		}
		t.Fatalf("last presence %s (all %v) while c2 is connected", s, all)
	}
}

// A caller whose context already ended (a client that hung up) must not turn
// a healthy Redis into a local-only, seq-less delivery.
func TestPublisher_CancelledCallerStillUsesBus(t *testing.T) {
	e := newRTEnv(t)
	a := e.connect(t, "ca", "A", "r1")
	ctx, cancel := context.WithCancel(bg)
	cancel()
	e.pub.Publish(ctx, stateEvent("r1", "A"))
	f := framesOf(a, EventMemberJoined)
	if len(f) != 1 || f[0].Seq != 1 {
		t.Fatalf("frames %+v", f)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fallbacks != 0 {
		t.Fatalf("onFallback called %d times for a healthy bus", e.fallbacks)
	}
}

func TestPresence_SweepAnnouncesDeadNodeUsersOffline(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	watcher := e.connect(t, "cw", owner, room)
	// The member's only connection was on a node that died: its entry is in
	// the store, never removed, and has expired.
	if _, err := e.store.Add(bg, member, "on-dead-node", time.Now().Add(-71*time.Second)); err != nil {
		t.Fatal(err)
	}
	e.presence.Sweep(bg)
	f := framesOf(watcher, EventPresenceUpdate)
	if len(f) != 1 {
		t.Fatalf("presence frames %d want 1", len(f))
	}
	if u, s := presenceOf(t, f[0]); u != member || s != model.PresenceOffline {
		t.Fatalf("presence %s %s", u, s)
	}
	e.presence.Sweep(bg)
	if n := len(framesOf(watcher, EventPresenceUpdate)); n != 1 {
		t.Fatalf("second sweep re-announced: frames=%d", n)
	}
}

func TestPresence_SweepSkipsUserConnectedHere(t *testing.T) {
	e := newRTEnv(t)
	owner, member, room := e.twoInRoom(t)
	watcher := e.connect(t, "cw", owner, room)
	m := e.connect(t, "cm", member)
	// A slow ping let the entry lapse, but the connection is still open here.
	if _, err := e.store.Add(bg, member, m.id, time.Now().Add(-71*time.Second)); err != nil {
		t.Fatal(err)
	}
	e.presence.Sweep(bg)
	for _, f := range framesOf(watcher, EventPresenceUpdate) {
		if _, s := presenceOf(t, f); s == model.PresenceOffline {
			t.Fatal("a user connected to this node must not be announced offline")
		}
	}
}
