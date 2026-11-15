package ratelimit

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
}

// failingCounter simulates an unreachable Redis.
type failingCounter struct{}

var errDown = errors.New("redis down")

func (failingCounter) Incr(context.Context, string, time.Duration) (Hit, error) {
	return Hit{}, errDown
}
func (failingCounter) SetFlag(context.Context, string, time.Duration) error { return errDown }
func (failingCounter) FlagTTL(context.Context, string) (time.Duration, error) {
	return 0, errDown
}
func (failingCounter) Delete(context.Context, string) error { return errDown }

func TestMemory_IncrFixedWindow(t *testing.T) {
	clk := newFakeClock()
	m := NewMemory(clk.now)
	ctx := context.Background()
	var last Hit
	for want := int64(1); want <= 3; want++ {
		h, err := m.Incr(ctx, "k", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if h.Count != want {
			t.Fatalf("count = %d, want %d", h.Count, want)
		}
		if h.Degraded {
			t.Fatal("memory hit must not be degraded")
		}
		clk.advance(10 * time.Second)
		last = h
	}
	h, _ := m.Incr(ctx, "k", time.Minute)
	if h.Resets >= last.Resets {
		t.Fatalf("Resets must shrink: %v then %v", last.Resets, h.Resets)
	}
	// 30s elapsed since the first hit, so 30s remain; later hits do not restart the window.
	if h.Resets != 30*time.Second {
		t.Fatalf("Resets = %v, want 30s", h.Resets)
	}
	clk.advance(31 * time.Second)
	h, _ = m.Incr(ctx, "k", time.Minute)
	if h.Count != 1 {
		t.Fatalf("count after window = %d, want 1", h.Count)
	}
	if h.Resets != time.Minute {
		t.Fatalf("Resets = %v, want 1m", h.Resets)
	}
}

func TestMemory_FlagAndDelete(t *testing.T) {
	clk := newFakeClock()
	m := NewMemory(clk.now)
	ctx := context.Background()
	if d, _ := m.FlagTTL(ctx, "f"); d != 0 {
		t.Fatalf("missing flag ttl = %v, want 0", d)
	}
	_ = m.SetFlag(ctx, "f", time.Hour)
	clk.advance(10 * time.Minute)
	if d, _ := m.FlagTTL(ctx, "f"); d != 50*time.Minute {
		t.Fatalf("ttl = %v, want 50m", d)
	}
	clk.advance(51 * time.Minute)
	if d, _ := m.FlagTTL(ctx, "f"); d != 0 {
		t.Fatalf("expired ttl = %v, want 0", d)
	}
	_, _ = m.Incr(ctx, "c", time.Hour)
	_ = m.Delete(ctx, "c")
	if h, _ := m.Incr(ctx, "c", time.Hour); h.Count != 1 {
		t.Fatalf("count after delete = %d, want 1", h.Count)
	}
}

func fail(g *JoinGuard, n int) {
	for i := 0; i < n; i++ {
		g.RecordFailure(context.Background(), "u1")
	}
}

func TestJoinGuard_CooldownAfterFiveFailures(t *testing.T) {
	clk := newFakeClock()
	g := NewJoinGuard(NewMemory(clk.now))
	ctx := context.Background()
	fail(g, 4)
	if d := g.Cooldown(ctx, "u1"); d != 0 {
		t.Fatalf("cooldown after 4 = %v, want 0", d)
	}
	fail(g, 1)
	d := g.Cooldown(ctx, "u1")
	if d <= 29*time.Minute || d > 30*time.Minute {
		t.Fatalf("cooldown = %v, want in (29m, 30m]", d)
	}
	if g.Cooldown(ctx, "u2") != 0 {
		t.Fatal("cooldown must be per user")
	}
	clk.advance(30 * time.Minute)
	if d := g.Cooldown(ctx, "u1"); d != 0 {
		t.Fatalf("cooldown after 30m = %v, want 0", d)
	}
	// The failure counter was cleared when the cooldown started.
	fail(g, 4)
	if d := g.Cooldown(ctx, "u1"); d != 0 {
		t.Fatalf("cooldown after fresh 4 = %v, want 0", d)
	}
}

func TestJoinGuard_FailuresExpireAfterWindow(t *testing.T) {
	clk := newFakeClock()
	g := NewJoinGuard(NewMemory(clk.now))
	fail(g, 4)
	clk.advance(11 * time.Minute)
	fail(g, 1)
	if d := g.Cooldown(context.Background(), "u1"); d != 0 {
		t.Fatalf("cooldown = %v, want 0", d)
	}
}

func TestJoinGuard_SuccessResetsFailures(t *testing.T) {
	clk := newFakeClock()
	g := NewJoinGuard(NewMemory(clk.now))
	fail(g, 4)
	g.RecordSuccess(context.Background(), "u1")
	fail(g, 4)
	if d := g.Cooldown(context.Background(), "u1"); d != 0 {
		t.Fatalf("cooldown = %v, want 0", d)
	}
}

func TestJoinGuard_IPLimit30(t *testing.T) {
	clk := newFakeClock()
	g := NewJoinGuard(NewMemory(clk.now))
	ctx := context.Background()
	for i := 1; i <= 30; i++ {
		if ok, _ := g.Allow(ctx, "1.2.3.4"); !ok {
			t.Fatalf("attempt %d denied", i)
		}
	}
	ok, retry := g.Allow(ctx, "1.2.3.4")
	if ok || retry <= 0 {
		t.Fatalf("31st: ok=%v retry=%v, want denied with retry>0", ok, retry)
	}
	if ok, _ := g.Allow(ctx, "5.6.7.8"); !ok {
		t.Fatal("other IP must be unaffected")
	}
	clk.advance(time.Hour + time.Second)
	if ok, _ := g.Allow(ctx, "1.2.3.4"); !ok {
		t.Fatal("window should have reset")
	}
}

func TestFallback_DegradedUsesTighterIPLimit(t *testing.T) {
	clk := newFakeClock()
	var calls int
	g := NewJoinGuard(NewFallback(failingCounter{}, NewMemory(clk.now), func(error) { calls++ }))
	ctx := context.Background()
	for i := 1; i <= 15; i++ {
		if ok, _ := g.Allow(ctx, "1.2.3.4"); !ok {
			t.Fatalf("attempt %d denied", i)
		}
	}
	if ok, retry := g.Allow(ctx, "1.2.3.4"); ok || retry <= 0 {
		t.Fatalf("16th: ok=%v retry=%v, want denied", ok, retry)
	}
	if calls == 0 {
		t.Fatal("onFallback was not called")
	}
}

func TestFallback_NeverReturnsError(t *testing.T) {
	clk := newFakeClock()
	f := NewFallback(failingCounter{}, NewMemory(clk.now), nil)
	ctx := context.Background()
	h, err := f.Incr(ctx, "k", time.Minute)
	if err != nil || !h.Degraded || h.Count != 1 {
		t.Fatalf("Incr = %+v, %v", h, err)
	}
	if err := f.SetFlag(ctx, "f", time.Minute); err != nil {
		t.Fatal(err)
	}
	if d, err := f.FlagTTL(ctx, "f"); err != nil || d <= 0 {
		t.Fatalf("FlagTTL = %v, %v", d, err)
	}
	if err := f.Delete(ctx, "f"); err != nil {
		t.Fatal(err)
	}
	if d, _ := f.FlagTTL(ctx, "f"); d != 0 {
		t.Fatalf("flag still present: %v", d)
	}
}

// timeoutCounter fails like a dial to a blackholed Redis: a net timeout that
// also satisfies errors.Is(err, context.DeadlineExceeded).
type timeoutCounter struct{}

// errDialTimeout is a real dial timeout: the deadline has passed before the
// dial starts, so no packet is sent and it returns at once.
var errDialTimeout = func() error {
	_, err := (&net.Dialer{Timeout: time.Nanosecond}).Dial("tcp", "10.255.255.1:6379")
	return err
}()

func (timeoutCounter) Incr(context.Context, string, time.Duration) (Hit, error) {
	return Hit{}, errDialTimeout
}
func (timeoutCounter) SetFlag(context.Context, string, time.Duration) error { return errDialTimeout }
func (timeoutCounter) FlagTTL(context.Context, string) (time.Duration, error) {
	return 0, errDialTimeout
}
func (timeoutCounter) Delete(context.Context, string) error { return errDialTimeout }

// Whether the caller gave up is read from the caller's context, never from the
// error: a dial timeout with a live context is an outage and must be reported.
func TestFallback_ReportsOnlyWhenCallerContextIsLive(t *testing.T) {
	if !errors.Is(errDialTimeout, context.DeadlineExceeded) {
		t.Fatal("test premise: a dial timeout satisfies errors.Is(context.DeadlineExceeded)")
	}
	clk := newFakeClock()
	var reported []error
	f := NewFallback(timeoutCounter{}, NewMemory(clk.now), func(err error) { reported = append(reported, err) })
	exercise := func(ctx context.Context) {
		t.Helper()
		if h, err := f.Incr(ctx, "k", time.Minute); err != nil || !h.Degraded {
			t.Fatalf("Incr = %+v, %v", h, err)
		}
		_ = f.SetFlag(ctx, "f", time.Minute)
		if d, _ := f.FlagTTL(ctx, "f"); d <= 0 {
			t.Fatalf("FlagTTL = %v: did not fall back", d)
		}
		_ = f.Delete(ctx, "f")
	}

	exercise(context.Background())
	if len(reported) != 4 || reported[0] != errDialTimeout {
		t.Fatalf("live context: reported %v, want 4 dial timeouts", reported)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	for _, ctx := range []context.Context{cancelled, expired} {
		reported = nil
		// The request is dead: zero values, no error, no fallback to the secondary...
		if h, err := f.Incr(ctx, "k2", time.Minute); err != nil || h != (Hit{Degraded: true}) {
			t.Fatalf("ended context Incr = %+v, %v", h, err)
		}
		if err := f.SetFlag(ctx, "f2", time.Minute); err != nil {
			t.Fatal(err)
		}
		if d, err := f.FlagTTL(ctx, "f2"); err != nil || d != 0 {
			t.Fatalf("ended context FlagTTL = %v, %v", d, err)
		}
		if err := f.Delete(ctx, "f2"); err != nil {
			t.Fatal(err)
		}
		if len(reported) != 0 { // ...and the caller's own cancellation is no outage
			t.Fatalf("ended context: reported %v", reported)
		}
	}
}

// A client that disconnects while Redis is healthy must not leave keys in the
// in-process counter, which Cooldown would never read while Redis is up.
func TestFallback_DeadContextLeavesSecondaryEmpty(t *testing.T) {
	clk := newFakeClock()
	mem := NewMemory(clk.now)
	f := NewFallback(timeoutCounter{}, mem, nil)
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = f.Incr(dead, "k", time.Minute)
	_, _ = f.Incr(dead, "k", time.Minute)
	_ = f.SetFlag(dead, "f", time.Hour)

	live := context.Background()
	if d, _ := mem.FlagTTL(live, "f"); d != 0 {
		t.Fatalf("flag written to secondary: %v", d)
	}
	if h, _ := mem.Incr(live, "k", time.Minute); h.Count != 1 {
		t.Fatalf("counter written to secondary: count=%d", h.Count)
	}
}

func TestFallback_UsesPrimaryWhenHealthy(t *testing.T) {
	clk := newFakeClock()
	f := NewFallback(NewMemory(clk.now), NewMemory(clk.now), func(error) { t.Fatal("unexpected fallback") })
	h, err := f.Incr(context.Background(), "k", time.Minute)
	if err != nil || h.Degraded {
		t.Fatalf("Incr = %+v, %v", h, err)
	}
}
