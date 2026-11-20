package realtime

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	pbredis "github.com/jinheng0301/petbridge/internal/platform/redis"
)

func testURL() string {
	if v := os.Getenv("TEST_REDIS_URL"); v != "" {
		return v
	}
	return "redis://127.0.0.1:6379/0"
}

// newTestRedis returns a client and a key prefix unique to this test; keys under
// it are deleted on cleanup.
func newTestRedis(t *testing.T) (*goredis.Client, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs redis")
	}
	c, err := pbredis.Open(context.Background(), testURL())
	if err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	prefix := fmt.Sprintf("test:rt:%s:%d:", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		keys, _ := c.Keys(ctx, prefix+"*").Result()
		if len(keys) > 0 {
			c.Del(ctx, keys...)
		}
		c.Close()
	})
	return c, prefix
}

// collector records messages and reconnects seen by Bus.Run.
type collector struct {
	mu         sync.Mutex
	msgs       []string
	reconnects atomic.Int32
}

func (c *collector) onMessage(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, string(b))
}

func (c *collector) has(msg string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runBus starts b.Run and keeps publishing probe until it arrives, so the
// subscription is known to be live when it returns.
func runBus(t *testing.T, b *Bus, col *collector) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx, col.onMessage, func() { col.reconnects.Add(1) }); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, 3*time.Second, "subscription", func() bool {
		_ = b.Publish(context.Background(), []byte("probe"))
		return col.has("probe")
	})
}

func TestBus_SeqIncrements(t *testing.T) {
	c, p := newTestRedis(t)
	b := NewBus(c, p+"ch", p)
	ctx := context.Background()
	if s, err := b.CurrentSeq(ctx, "r1"); err != nil || s != 0 {
		t.Fatalf("CurrentSeq before any event = %d, %v", s, err)
	}
	for want := int64(1); want <= 2; want++ {
		if s, err := b.NextSeq(ctx, "r1"); err != nil || s != want {
			t.Fatalf("NextSeq = %d, %v; want %d", s, err, want)
		}
	}
	if s, err := b.CurrentSeq(ctx, "r1"); err != nil || s != 2 {
		t.Fatalf("CurrentSeq = %d, %v; want 2", s, err)
	}
	if n, _ := c.Exists(ctx, p+"room:r1:seq").Result(); n != 1 {
		t.Fatal("seq key must be room:{rid}:seq under the prefix")
	}
}

func TestBus_PublishReachesRun(t *testing.T) {
	c, p := newTestRedis(t)
	b := NewBus(c, p+"ch", p)
	col := &collector{}
	runBus(t, b, col)
	if err := b.Publish(context.Background(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, `message "x"`, func() bool { return col.has("x") })
	if n := col.reconnects.Load(); n != 0 {
		t.Fatalf("first subscription must not count as a reconnect, got %d", n)
	}
}

func TestBus_ReconnectCallback(t *testing.T) {
	c, p := newTestRedis(t)
	b := NewBus(c, p+"ch", p)
	col := &collector{}
	runBus(t, b, col)

	if err := c.Do(context.Background(), "CLIENT", "KILL", "TYPE", "pubsub").Err(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "reconnect callback", func() bool { return col.reconnects.Load() >= 1 })
	time.Sleep(200 * time.Millisecond)
	if n := col.reconnects.Load(); n != 1 {
		t.Fatalf("reconnects = %d, want 1", n)
	}
	waitFor(t, 3*time.Second, "message after reconnect", func() bool {
		_ = b.Publish(context.Background(), []byte("after"))
		return col.has("after")
	})
}

func TestPresence_AddRemoveReturnCounts(t *testing.T) {
	c, p := newTestRedis(t)
	ps := NewPresenceStore(c, p, 70*time.Second)
	ctx := context.Background()
	now := time.Now()
	for i, conn := range []string{"c1", "c2"} {
		if n, err := ps.Add(ctx, "u1", conn, now); err != nil || n != i+1 {
			t.Fatalf("Add(%s) = %d, %v; want %d", conn, n, err, i+1)
		}
	}
	if n, err := ps.Add(ctx, "u1", "c2", now); err != nil || n != 2 {
		t.Fatalf("refresh Add = %d, %v; want 2", n, err)
	}
	if n, err := ps.Count(ctx, "u1", now); err != nil || n != 2 {
		t.Fatalf("Count = %d, %v; want 2", n, err)
	}
	if n, err := ps.Remove(ctx, "u1", "c1", now); err != nil || n != 1 {
		t.Fatalf("Remove(c1) = %d, %v; want 1", n, err)
	}
	if n, err := ps.Count(ctx, "u1", now.Add(71*time.Second)); err != nil || n != 0 {
		t.Fatalf("Count after expiry = %d, %v; want 0", n, err)
	}
	if n, err := ps.Remove(ctx, "u1", "c2", now); err != nil || n != 0 {
		t.Fatalf("Remove(c2) = %d, %v; want 0", n, err)
	}
}

func TestPresence_KeyHasTTL(t *testing.T) {
	c, p := newTestRedis(t)
	ps := NewPresenceStore(c, p, 70*time.Second)
	ctx := context.Background()
	if _, err := ps.Add(ctx, "u1", "c1", time.Now()); err != nil {
		t.Fatal(err)
	}
	ttl, err := c.PTTL(ctx, p+"presence:u:u1").Result()
	if err != nil {
		t.Fatal(err)
	}
	// Twice the member TTL, so Sweep can still find a dead node's members.
	if ttl <= 70*time.Second || ttl > 140*time.Second {
		t.Fatalf("PTTL = %v, want (70s, 140s]", ttl)
	}
}

func TestPresence_SweepReportsEachExpiredUserOnce(t *testing.T) {
	c, p := newTestRedis(t)
	ps := NewPresenceStore(c, p, 70*time.Second)
	ctx := context.Background()
	now := time.Now()
	for _, a := range [][2]string{{"dead", "c1"}, {"dead", "c2"}, {"alive", "c3"}, {"closed", "c4"}} {
		if _, err := ps.Add(ctx, a[0], a[1], now); err != nil {
			t.Fatal(err)
		}
	}
	later := now.Add(60 * time.Second)
	if _, err := ps.Add(ctx, "alive", "c3", later); err != nil { // still pinging
		t.Fatal(err)
	}
	if _, err := ps.Remove(ctx, "closed", "c4", now); err != nil { // closed normally
		t.Fatal(err)
	}
	if got, err := ps.Sweep(ctx, now.Add(30*time.Second)); err != nil || len(got) != 0 {
		t.Fatalf("early Sweep = %v, %v; want nothing", got, err)
	}
	if n, _ := ps.Count(ctx, "dead", now.Add(71*time.Second)); n != 0 {
		t.Fatalf("Count after expiry = %d, want 0", n)
	}
	got, err := ps.Sweep(ctx, now.Add(71*time.Second))
	if err != nil || len(got) != 1 || got[0] != "dead" {
		t.Fatalf("Sweep = %v, %v; want [dead]", got, err)
	}
	if got, err := ps.Sweep(ctx, now.Add(72*time.Second)); err != nil || len(got) != 0 {
		t.Fatalf("second Sweep = %v, %v; want nothing (already reported)", got, err)
	}
}
