package aistore

import (
	"context"
	"fmt"
	"net"
	"os"
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

// newTestRedis returns a client and a token unique to this test. Every key the
// test creates contains the token; they are deleted on cleanup.
func newTestRedis(t *testing.T) (*goredis.Client, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs redis")
	}
	c, err := pbredis.Open(context.Background(), testURL())
	if err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	token := fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
	cleaner, _ := pbredis.New(testURL()) // c may be closed by the test
	t.Cleanup(func() {
		ctx := context.Background()
		keys, _ := cleaner.Keys(ctx, "*"+token+"*").Result()
		if len(keys) > 0 {
			cleaner.Del(ctx, keys...)
		}
		cleaner.Close()
		c.Close()
	})
	return c, token
}

func TestAIStore_CacheRoundTrip(t *testing.T) {
	c, token := newTestRedis(t)
	cache := NewCache(c)
	ctx := context.Background()
	key := "tr:" + token
	if _, ok, err := cache.Get(ctx, key); err != nil || ok {
		t.Fatalf("missing key: ok=%v err=%v", ok, err)
	}
	if err := cache.Set(ctx, key, "hello", time.Hour); err != nil {
		t.Fatal(err)
	}
	v, ok, err := cache.Get(ctx, key)
	if err != nil || !ok || v != "hello" {
		t.Fatalf("got %q ok=%v err=%v", v, ok, err)
	}
	if ttl, _ := c.PTTL(ctx, key).Result(); ttl <= 59*time.Minute || ttl > time.Hour {
		t.Fatalf("ttl = %v", ttl)
	}
	c.Close()
	if _, _, err := cache.Get(ctx, key); err == nil {
		t.Fatal("Get on a closed client: want error")
	}
	if err := cache.Set(ctx, key, "x", time.Hour); err == nil {
		t.Fatal("Set on a closed client: want error")
	}
}

func TestAIStore_CostMeterFallsBack(t *testing.T) {
	c, day := newTestRedis(t)
	var fallbacks atomic.Int64
	m := NewCostMeter(c, func(error) { fallbacks.Add(1) })
	ctx := context.Background()
	if err := m.Add(ctx, day, 100); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(ctx, day, 250); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Total(ctx, day); err != nil || got != 350 {
		t.Fatalf("total = %d, %v; want 350", got, err)
	}
	ttl, err := c.PTTL(ctx, "ai:cost:"+day).Result()
	if err != nil || ttl <= 0 || ttl > 48*time.Hour {
		t.Fatalf("key ttl = %v, %v", ttl, err)
	}
	if got, _ := m.Total(ctx, day+"-other"); got != 0 {
		t.Fatalf("unknown day total = %d", got)
	}
	if fallbacks.Load() != 0 {
		t.Fatalf("fell back while Redis was up")
	}

	c.Close()
	if err := m.Add(ctx, day, 40); err != nil {
		t.Fatalf("Add with Redis down: %v", err)
	}
	if err := m.Add(ctx, day, 2); err != nil {
		t.Fatalf("Add with Redis down: %v", err)
	}
	got, err := m.Total(ctx, day)
	if err != nil || got != 42 {
		t.Fatalf("in-process total = %d, %v; want 42", got, err)
	}
	if fallbacks.Load() == 0 {
		t.Fatal("onFallback not called")
	}
}

func TestAIStore_CostMeterNilFallbackAndDeadServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	c, err := pbredis.New("redis://" + addr + "/0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	m := NewCostMeter(c, nil)
	ctx := context.Background()
	if err := m.Add(ctx, "20261121", 7); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Total(ctx, "20261121"); err != nil || got != 7 {
		t.Fatalf("total = %d, %v; want 7", got, err)
	}
	if got, _ := m.Total(ctx, "20261122"); got != 0 {
		t.Fatalf("other day = %d", got)
	}
}
