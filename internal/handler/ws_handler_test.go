package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/middleware"
	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/server"
	"github.com/jinheng0301/petbridge/internal/service"
	"github.com/jinheng0301/petbridge/pkg/idtoken"
)

var errBusDown = errors.New("bus down")

// wsBus is an in-process service.Bus (the service package's fake is not
// visible here): Publish hands the payload to the handler registered by Run;
// fail makes every call error, as a dead Redis would.
type wsBus struct {
	mu    sync.Mutex
	fail  bool
	seq   map[string]int64
	onMsg func([]byte)
	ready chan struct{}
	once  sync.Once
}

func (b *wsBus) setFail(v bool) { b.mu.Lock(); b.fail = v; b.mu.Unlock() }

func (b *wsBus) NextSeq(_ context.Context, room string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return 0, errBusDown
	}
	b.seq[room]++
	return b.seq[room], nil
}

func (b *wsBus) CurrentSeq(_ context.Context, room string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail {
		return 0, errBusDown
	}
	return b.seq[room], nil
}

func (b *wsBus) Publish(_ context.Context, p []byte) error {
	b.mu.Lock()
	fail, h := b.fail, b.onMsg
	b.mu.Unlock()
	if fail {
		return errBusDown
	}
	h(p)
	return nil
}

func (b *wsBus) Run(ctx context.Context, onMessage func([]byte), _ func()) {
	b.mu.Lock()
	b.onMsg = onMessage
	b.mu.Unlock()
	b.once.Do(func() { close(b.ready) })
	<-ctx.Done()
}

// memStore is an in-process service.PresenceStore that logs every change.
type memStore struct {
	mu    sync.Mutex
	conns map[string]map[string]bool
	ops   []string // "add <conn>" / "remove <conn>" in call order
}

func (s *memStore) Add(_ context.Context, u, c string, _ time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[u] == nil {
		s.conns[u] = map[string]bool{}
	}
	s.conns[u][c] = true
	s.ops = append(s.ops, "add "+c)
	return len(s.conns[u]), nil
}

func (s *memStore) Remove(_ context.Context, u, c string, _ time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns[u], c)
	s.ops = append(s.ops, "remove "+c)
	return len(s.conns[u]), nil
}

// addedAfterRemove lists connections that were added again after their removal.
func (s *memStore) addedAfterRemove() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed, bad := map[string]bool{}, []string{}
	for _, op := range s.ops {
		kind, conn, _ := strings.Cut(op, " ")
		if kind == "remove" {
			removed[conn] = true
		} else if removed[conn] {
			bad = append(bad, conn)
		}
	}
	return bad
}

func (s *memStore) Count(_ context.Context, u string, _ time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns[u]), nil
}

// fakeExpiry stands in for service.Issuer.Expiry: every token expires d after
// the upgrade, so tests can make it short.
type fakeExpiry struct {
	mu sync.Mutex
	d  time.Duration
}

func (f *fakeExpiry) set(d time.Duration) { f.mu.Lock(); f.d = d; f.mu.Unlock() }

func (f *fakeExpiry) Expiry(string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Now().Add(f.d), nil
}

// wsOpts tweaks newWSApp; zero values mean production defaults.
type wsOpts struct {
	cfg         *service.RealtimeConfig
	readTimeout time.Duration
	userLimit   int // upgrades per user per minute
}

// wsApp is the full router with realtime wired as main.go does it, served
// over a real HTTP server.
type wsApp struct {
	*app
	srv   *httptest.Server
	bus   *wsBus
	ws    *handler.WsHandler
	store *memStore
	exp   *fakeExpiry
}

func newWSApp(t *testing.T, o wsOpts) *wsApp {
	t.Helper()
	cfg := service.DefaultRealtimeConfig("1.2.3")
	if o.cfg != nil {
		cfg = *o.cfg
	}
	if o.readTimeout == 0 {
		o.readTimeout = 15 * time.Second
	}
	if o.userLimit == 0 {
		o.userLimit = 1000
	}
	exp := &fakeExpiry{d: time.Hour}
	authSvc, userSvc, iss, pool := newServices(t)
	counter := ratelimit.NewMemory(time.Now)
	users := repository.NewUserRepo(pool)
	rooms := repository.NewRoomRepo(pool)
	members := repository.NewRoomMemberRepo(pool)
	codes := repository.NewInviteCodeRepo(pool)

	bus := &wsBus{seq: map[string]int64{}, ready: make(chan struct{})}
	hub := service.NewHub(cfg)
	pub := service.NewEventPublisher(bus, hub, nil)
	store := &memStore{conns: map[string]map[string]bool{}}
	presence := service.NewPresenceService(store, hub, rooms, pub, cfg, time.Now)
	pub.SetPresence(presence)
	rt := service.NewRealtimeService(rooms, members, presence, pub, pub)
	ctx, cancel := context.WithCancel(context.Background())
	go pub.Run(ctx)
	<-bus.ready

	joinSvc := service.NewJoinService(users, rooms, members, codes, repository.NewBanRepo(pool),
		repository.NewRewardRepo(pool), repository.NewTxManager(pool), ratelimit.NewJoinGuard(counter), pub,
		service.JoinConfig{CodeTTLDays: 7, RewardCount: 3, RewardLifetimeCap: 20}, time.Now)
	manageSvc := service.NewManageService(users, rooms, members, codes, repository.NewBanRepo(pool),
		repository.NewTxManager(pool), pub,
		service.ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 3, LifetimeCap: 20}, time.Now)
	authn := middleware.Auth(iss, userSvc)
	ws := handler.NewWsHandler(hub, presence, rt, users, exp, cfg, authn).
		WithUpgradeLimit(middleware.RateLimitIP(counter, "ws", 1000, time.Minute),
			middleware.RateLimitUser(counter, "ws", o.userLimit, time.Minute))
	logs := &bytes.Buffer{}
	r := server.NewRouter(logger.New("debug", logs), handler.NewHealthHandler(okProbe, okProbe),
		handler.NewAuthHandler(authSvc, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{}).
			WithLoginLimit(middleware.RateLimitIP(counter, "login", 10, time.Minute)),
		handler.NewUserHandler(userSvc, authn),
		handler.NewRoomHandler(joinSvc, service.NewRoomService(rooms, users, codes, 3, time.Now), authn).WithManage(manageSvc),
		handler.NewCodeHandler(service.NewCodeService(codes, time.Now), authn),
		ws)

	srv := httptest.NewUnstartedServer(r)
	srv.Config.ReadTimeout = o.readTimeout
	srv.Start()
	t.Cleanup(func() { ws.Shutdown(5 * time.Second); srv.Close(); cancel() })
	return &wsApp{app: &app{r: r, pool: pool, logs: logs}, srv: srv, bus: bus, ws: ws, store: store, exp: exp}
}

func defaultWSApp(t *testing.T) *wsApp {
	return newWSApp(t, wsOpts{})
}

// inFrame is a frame as the client receives it.
type inFrame struct {
	T   string         `json:"t"`
	Seq int64          `json:"seq"`
	D   map[string]any `json:"d"`
}

// wsClient reads frames in the background into a channel.
type wsClient struct {
	t      *testing.T
	c      *websocket.Conn
	frames chan inFrame
	done   chan struct{}
	err    error // read error, set before done closes
}

func (a *wsApp) dialRaw(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(a.srv.URL, "http")+"/api/v1/ws", &websocket.DialOptions{HTTPHeader: h})
}

func (a *wsApp) dial(t *testing.T, token string) *wsClient {
	t.Helper()
	c, _, err := a.dialRaw(t, token)
	if err != nil {
		t.Fatal(err)
	}
	cl := &wsClient{t: t, c: c, frames: make(chan inFrame, 256), done: make(chan struct{})}
	go func() {
		defer close(cl.done)
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				cl.err = err
				return
			}
			var f inFrame
			if json.Unmarshal(b, &f) == nil {
				cl.frames <- f
			}
		}
	}()
	t.Cleanup(func() { c.CloseNow() })
	cl.expect("hello")
	return cl
}

func (cl *wsClient) send(v any) {
	cl.t.Helper()
	b, _ := json.Marshal(v)
	cl.sendRaw(websocket.MessageText, b)
}

func (cl *wsClient) sendRaw(typ websocket.MessageType, b []byte) {
	cl.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := cl.c.Write(ctx, typ, b); err != nil {
		cl.t.Fatal(err)
	}
}

// expect returns the next frame of type typ, skipping others.
func (cl *wsClient) expect(typ string) inFrame {
	cl.t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case f := <-cl.frames:
			if f.T == typ {
				return f
			}
		case <-timeout:
			cl.t.Fatalf("no %s frame within 3s", typ)
		}
	}
}

// none fails if a frame of type typ arrives within d.
func (cl *wsClient) none(typ string, d time.Duration) {
	cl.t.Helper()
	timeout := time.After(d)
	for {
		select {
		case f := <-cl.frames:
			if f.T == typ {
				cl.t.Fatalf("unexpected %s frame: %+v", typ, f)
			}
		case <-timeout:
			return
		}
	}
}

// closeStatus waits for the server to close the connection.
func (cl *wsClient) closeStatus() websocket.StatusCode {
	cl.t.Helper()
	select {
	case <-cl.done:
		return websocket.CloseStatus(cl.err)
	case <-time.After(3 * time.Second):
		cl.t.Fatal("connection not closed within 3s")
		return 0
	}
}

func (cl *wsClient) sub(room string) inFrame {
	cl.t.Helper()
	cl.send(map[string]any{"t": "room.sub", "id": "s-" + room, "d": map[string]any{"room_id": room}})
	return cl.expect("room.snapshot")
}

func TestWS_RequiresAuth(t *testing.T) {
	a := defaultWSApp(t)
	if _, resp, err := a.dialRaw(t, ""); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Fatalf("no token: err=%v resp=%v", err, resp)
	}
	bare := a.bare(t, "bare@x.test")
	_, resp, err := a.dialRaw(t, bare.token)
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("not onboarded: err=%v resp=%v", err, resp)
	}
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error.Code != model.CodeOnboardingRequired {
		t.Fatalf("code=%q", body.Error.Code)
	}
}

func TestWS_HelloAndSnapshot(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	c, _, err := a.dialRaw(t, o.token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	_, b, err := c.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var hello inFrame
	if err := json.Unmarshal(b, &hello); err != nil || hello.T != "hello" {
		t.Fatalf("first frame %s", b)
	}
	if hello.D["proto"] != float64(1) || hello.D["heartbeat_sec"] != float64(25) || hello.D["min_app_version"] != "1.2.3" {
		t.Fatalf("hello %v", hello.D)
	}
	if _, err := time.Parse(time.RFC3339, hello.D["server_time"].(string)); err != nil {
		t.Fatalf("server_time %v", hello.D["server_time"])
	}
	c.CloseNow()

	cl := a.dial(t, o.token)
	snap := cl.sub(strings.ToUpper(o.roomID))
	room := snap.D["room"].(map[string]any)
	members := room["members"].([]any)
	if snap.D["room_id"] != o.roomID || len(members) != 1 || room["is_owner"] != true {
		t.Fatalf("snapshot %v", snap.D)
	}
	if p := members[0].(map[string]any)["presence"]; p != "online" {
		t.Fatalf("own presence %v", p)
	}
}

func TestWS_SubOthersRoom(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	s := a.onboarded(t, "s@x.test", "18_plus")
	cl := a.dial(t, s.token)
	cl.send(map[string]any{"t": "room.sub", "id": "x1", "d": map[string]any{"room_id": o.roomID}})
	e := cl.expect("error")
	if e.D["ref"] != "x1" || e.D["code"] != model.CodeRoomNotFound {
		t.Fatalf("error %v", e.D)
	}
	cl.sub(s.roomID) // still usable
}

func TestWS_JoinPushesMemberJoined(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	b := a.onboarded(t, "b@x.test", "18_plus")
	cl := a.dial(t, o.token)
	cl.sub(o.roomID)
	if w := a.join(b, o.code); w.Code != 200 {
		t.Fatalf("join %d %s", w.Code, w.Body.String())
	}
	f := cl.expect("member.joined")
	m := f.D["member"].(map[string]any)
	if f.Seq <= 0 || m["user_id"] != b.id || m["presence"] != "offline" {
		t.Fatalf("member.joined seq=%d d=%v", f.Seq, f.D)
	}
}

func TestWS_KickedStopsEvents(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	b := a.onboarded(t, "b@x.test", "18_plus")
	c := a.onboarded(t, "c@x.test", "18_plus")
	a.join(b, o.code)
	bc := a.dial(t, b.token)
	bc.sub(o.roomID)

	if w := call(a.r, "DELETE", "/api/v1/rooms/"+o.roomID+"/members/"+b.id, "", o.token); w.Code != 200 {
		t.Fatalf("remove %d %s", w.Code, w.Body.String())
	}
	if k := bc.expect("room.kicked"); k.D["reason"] != "kicked" || k.D["room_id"] != o.roomID {
		t.Fatalf("room.kicked %v", k.D)
	}
	if w := a.join(c, a.extraCode(t, o, "CCCC2345")); w.Code != 200 {
		t.Fatalf("join %d %s", w.Code, w.Body.String())
	}
	bc.none("member.joined", 300*time.Millisecond)
}

func TestWS_LeaveNotifiesOtherDevice(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	b := a.onboarded(t, "b@x.test", "18_plus")
	a.join(b, o.code)
	d1, d2 := a.dial(t, b.token), a.dial(t, b.token)
	if w := call(a.r, "POST", "/api/v1/rooms/"+o.roomID+"/leave", "", b.token); w.Code != 204 {
		t.Fatalf("leave %d %s", w.Code, w.Body.String())
	}
	for _, d := range []*wsClient{d1, d2} {
		if k := d.expect("room.kicked"); k.D["reason"] != "left" {
			t.Fatalf("room.kicked %v", k.D)
		}
	}
}

func TestWS_PetTapRelayed(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	b := a.onboarded(t, "b@x.test", "18_plus")
	a.join(b, o.code)
	oc, bc := a.dial(t, o.token), a.dial(t, b.token)
	oc.sub(o.roomID)
	bc.sub(o.roomID)

	oc.send(map[string]any{"t": "pet.tap", "id": "t1", "d": map[string]any{"room_id": o.roomID, "target_user_id": b.id}})
	f := bc.expect("pet.action")
	if f.D["from_user_id"] != o.id || f.D["target_user_id"] != b.id || f.D["action"] != "tap" || f.Seq != 0 {
		t.Fatalf("pet.action %+v", f)
	}
	oc.none("pet.action", 300*time.Millisecond)

	// A tap on a room the connection has not subscribed is refused.
	s := a.onboarded(t, "s@x.test", "18_plus")
	sc := a.dial(t, s.token)
	sc.send(map[string]any{"t": "pet.tap", "id": "t2", "d": map[string]any{"room_id": o.roomID, "target_user_id": b.id}})
	if e := sc.expect("error"); e.D["code"] != model.CodeRoomNotFound || e.D["ref"] != "t2" {
		t.Fatalf("error %v", e.D)
	}
}

func TestWS_PresenceWake(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	b := a.onboarded(t, "b@x.test", "18_plus")
	a.join(b, o.code)
	oc := a.dial(t, o.token)
	oc.sub(o.roomID)

	bc := a.dial(t, b.token)
	if p := oc.expect("presence.update"); p.D["user_id"] != b.id || p.D["status"] != "online" {
		t.Fatalf("presence %v", p.D)
	}
	bc.c.Close(websocket.StatusNormalClosure, "bye")
	if p := oc.expect("presence.update"); p.D["status"] != "offline" {
		t.Fatalf("presence %v", p.D)
	}
	a.dial(t, b.token)
	if p := oc.expect("presence.update"); p.D["status"] != "online" {
		t.Fatalf("presence %v", p.D)
	}
}

func TestWS_RedisDownStillDelivers(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	b := a.onboarded(t, "b@x.test", "18_plus")
	cl := a.dial(t, o.token)
	cl.sub(o.roomID)
	a.bus.setFail(true)
	a.join(b, o.code)
	if f := cl.expect("member.joined"); f.Seq != 0 || f.D["member"].(map[string]any)["user_id"] != b.id {
		t.Fatalf("member.joined %+v", f)
	}
}

func TestWS_MalformedFrames(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	cl := a.dial(t, o.token)
	bad := []struct {
		typ  websocket.MessageType
		body string
	}{
		{websocket.MessageText, "not json"},
		{websocket.MessageBinary, `{"t":"room.sub"}`},
		{websocket.MessageText, `{"t":"room.sub","id":"b3","d":"x"}`},
		{websocket.MessageText, `{"t":"room.sub","id":"b4","d":{}}`},
		{websocket.MessageText, `{"t":"nope","id":"b5"}`},
	}
	for _, b := range bad {
		cl.sendRaw(b.typ, []byte(b.body))
		if e := cl.expect("error"); e.D["code"] != model.CodeValidationFailed {
			t.Fatalf("%q: error %v", b.body, e.D)
		}
	}
	cl.sub(o.roomID) // still usable

	big := a.dial(t, o.token)
	big.sendRaw(websocket.MessageText, []byte(`{"t":"x","d":"`+strings.Repeat("a", 5000)+`"}`))
	if s := big.closeStatus(); s != websocket.StatusMessageTooBig {
		t.Fatalf("oversized frame close status %d", s)
	}

	flood := a.dial(t, o.token)
	for i := 0; i < 11; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = flood.c.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"t":"room.unsub","id":"f%d","d":{"room_id":"x"}}`, i)))
		cancel()
	}
	if s := flood.closeStatus(); s != websocket.StatusPolicyViolation {
		t.Fatalf("flood close status %d", s)
	}
}

func TestWS_IdleConnSurvivesReadTimeout(t *testing.T) {
	a := newWSApp(t, wsOpts{readTimeout: 300 * time.Millisecond})
	o := a.onboarded(t, "o@x.test", "18_plus")
	cl := a.dial(t, o.token)
	time.Sleep(time.Second)
	cl.sub(o.roomID)
}

func TestWS_DeadClientGoesOffline(t *testing.T) {
	cfg := service.DefaultRealtimeConfig("0.0.0")
	cfg.Heartbeat, cfg.PingTimeout = 100*time.Millisecond, 100*time.Millisecond
	a := newWSApp(t, wsOpts{cfg: &cfg})
	o := a.onboarded(t, "o@x.test", "18_plus")
	b := a.onboarded(t, "b@x.test", "18_plus")
	a.join(b, o.code)
	oc := a.dial(t, o.token)
	oc.sub(o.roomID)

	// B never reads, so its client never answers pings.
	dead, _, err := a.dialRaw(t, b.token)
	if err != nil {
		t.Fatal(err)
	}
	defer dead.CloseNow()
	if p := oc.expect("presence.update"); p.D["status"] != "online" {
		t.Fatalf("presence %v", p.D)
	}
	if p := oc.expect("presence.update"); p.D["user_id"] != b.id || p.D["status"] != "offline" {
		t.Fatalf("presence %v", p.D)
	}
}


func TestWS_ShutdownWaitsForConnections(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	cl := a.dial(t, o.token)
	cl.sub(o.roomID)

	a.ws.Shutdown(5 * time.Second)

	// Shutdown returns only after each connection has finished, presence included.
	if n, _ := a.store.Count(bg, o.id, time.Now()); n != 0 {
		t.Fatalf("presence still has %d connection(s) after Shutdown returned", n)
	}
	cl.expect("server.restart")
	if s := cl.closeStatus(); s != websocket.StatusGoingAway {
		t.Fatalf("close status %d, want 1001", s)
	}
}

// A heartbeat refresh must never land after the connection's presence was removed.
func TestWS_NoRefreshAfterDisconnect(t *testing.T) {
	cfg := service.DefaultRealtimeConfig("0.0.0")
	cfg.Heartbeat, cfg.PingTimeout = time.Millisecond, 200*time.Millisecond
	a := newWSApp(t, wsOpts{cfg: &cfg})
	o := a.onboarded(t, "o@x.test", "18_plus")
	for i := 0; i < 40; i++ {
		cl := a.dial(t, o.token)
		time.Sleep(3 * time.Millisecond)
		cl.c.Close(websocket.StatusNormalClosure, "")
	}
	a.ws.Shutdown(5 * time.Second)
	if bad := a.store.addedAfterRemove(); len(bad) > 0 {
		t.Fatalf("refreshed after removal: %v", bad)
	}
}

func TestWS_TokenExpiryCloses(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	a.exp.set(300 * time.Millisecond)
	cl := a.dial(t, o.token)
	if s := cl.closeStatus(); s != model.CloseTokenExpired {
		t.Fatalf("close status %d, want %d", s, model.CloseTokenExpired)
	}
}

func TestWS_UpgradeRateLimitedPerUser(t *testing.T) {
	a := newWSApp(t, wsOpts{userLimit: 2})
	o := a.onboarded(t, "o@x.test", "18_plus")
	a.dial(t, o.token)
	a.dial(t, o.token)
	_, resp, err := a.dialRaw(t, o.token)
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third upgrade: err=%v resp=%v", err, resp)
	}
}

func TestWS_RoomSubRateLimited(t *testing.T) {
	cfg := service.DefaultRealtimeConfig("0.0.0")
	cfg.SubsPerWindow = 2
	a := newWSApp(t, wsOpts{cfg: &cfg})
	o := a.onboarded(t, "o@x.test", "18_plus")
	cl := a.dial(t, o.token)
	cl.sub(o.roomID)
	cl.sub(o.roomID)
	cl.send(map[string]any{"t": "room.sub", "id": "s3", "d": map[string]any{"room_id": o.roomID}})
	if e := cl.expect("error"); e.D["code"] != model.CodeRateLimited || e.D["ref"] != "s3" {
		t.Fatalf("error %v", e.D)
	}
}

func TestWS_NonUUIDRoomIsNotFound(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	cl := a.dial(t, o.token)
	cl.send(map[string]any{"t": "room.sub", "id": "u1", "d": map[string]any{"room_id": strings.Repeat("x", 3000)}})
	if e := cl.expect("error"); e.D["code"] != model.CodeRoomNotFound {
		t.Fatalf("error %v", e.D)
	}
}

func TestWS_ShutdownRefusesNewUpgrades(t *testing.T) {
	a := defaultWSApp(t)
	o := a.onboarded(t, "o@x.test", "18_plus")
	a.ws.Shutdown(time.Second)
	_, resp, err := a.dialRaw(t, o.token)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("upgrade during shutdown: err=%v resp=%v", err, resp)
	}
}
