package ratelimit

import (
	"context"
	"fmt"
	"net"
	"os"
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
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("test:rl:%s:%d:", t.Name(), time.Now().UnixNano())
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

func TestRedis_IncrSetsExpiryAtomically(t *testing.T) {
	c, p := newTestRedis(t)
	r := NewRedis(c)
	ctx := context.Background()
	for want := int64(1); want <= 3; want++ {
		h, err := r.Incr(ctx, p+"k", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if h.Count != want || h.Degraded {
			t.Fatalf("hit = %+v, want count %d", h, want)
		}
		if h.Resets <= 0 || h.Resets > time.Minute {
			t.Fatalf("Resets = %v", h.Resets)
		}
	}
	ttl, err := c.PTTL(ctx, p+"k").Result()
	if err != nil || ttl <= 0 || ttl > time.Minute {
		t.Fatalf("key ttl = %v, %v", ttl, err)
	}
}

func TestRedis_FlagTTL(t *testing.T) {
	c, p := newTestRedis(t)
	r := NewRedis(c)
	ctx := context.Background()
	if d, err := r.FlagTTL(ctx, p+"f"); err != nil || d != 0 {
		t.Fatalf("missing flag = %v, %v", d, err)
	}
	if err := r.SetFlag(ctx, p+"f", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	d, err := r.FlagTTL(ctx, p+"f")
	if err != nil || d <= 29*time.Minute || d > 30*time.Minute {
		t.Fatalf("ttl = %v, %v", d, err)
	}
	if err := r.Delete(ctx, p+"f"); err != nil {
		t.Fatal(err)
	}
	if d, _ := r.FlagTTL(ctx, p+"f"); d != 0 {
		t.Fatalf("after delete ttl = %v", d)
	}
}

func TestRedis_ErrorWhenServerDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens here any more
	c, err := pbredis.New("redis://" + addr + "/0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := NewRedis(c)
	start := time.Now()
	if _, err := r.Incr(context.Background(), "k", time.Minute); err == nil {
		t.Fatal("expected error")
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("returned after %v, want within ~2s", d)
	}
}
