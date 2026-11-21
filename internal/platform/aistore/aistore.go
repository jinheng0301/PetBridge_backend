// Package aistore holds the Redis-backed stores of the AI gateway: the
// translation cache and the daily cost meter.
package aistore

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Cache is the translation cache (service.AICache). Errors are returned; the
// gateway treats them as a miss. Build the client with the platform/redis
// package so calls fail within its 2 second timeouts.
type Cache struct{ c *goredis.Client }

func NewCache(rdb *goredis.Client) *Cache { return &Cache{c: rdb} }

func (c *Cache) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := c.c.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (c *Cache) Set(ctx context.Context, key, val string, ttl time.Duration) error {
	return c.c.Set(ctx, key, val, ttl).Err()
}

const (
	costKeyPrefix = "ai:cost:"
	costKeyTTL    = 48 * time.Hour
	memDaysKept   = 2
)

// CostMeter (service.CostMeter) keeps the day's spend in ai:cost:{day} with a
// 48h TTL. When Redis fails it counts in process instead and never returns an
// error: what Redis missed is added to the Redis total once it is back, so a
// short outage does not hide spend from this process.
type CostMeter struct {
	c          *goredis.Client
	onFallback func(error)

	mu  sync.Mutex
	mem map[string]int64 // day -> micros Redis did not get
}

// NewCostMeter builds the meter. onFallback (may be nil) is told why a call
// fell back, for logging; it is not called when the caller's ctx has ended.
func NewCostMeter(rdb *goredis.Client, onFallback func(error)) *CostMeter {
	return &CostMeter{c: rdb, onFallback: onFallback, mem: make(map[string]int64)}
}

func (m *CostMeter) failed(ctx context.Context, err error) {
	if m.onFallback != nil && ctx.Err() == nil {
		m.onFallback(err)
	}
}

func (m *CostMeter) Add(ctx context.Context, day string, micros int64) error {
	key := costKeyPrefix + day
	_, err := m.c.TxPipelined(ctx, func(p goredis.Pipeliner) error {
		p.IncrBy(ctx, key, micros)
		p.Expire(ctx, key, costKeyTTL)
		return nil
	})
	if err == nil {
		return nil
	}
	m.failed(ctx, err)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mem[day] += micros
	m.prune()
	return nil
}

func (m *CostMeter) Total(ctx context.Context, day string) (int64, error) {
	v, err := m.c.Get(ctx, costKeyPrefix+day).Int64()
	if errors.Is(err, goredis.Nil) {
		v, err = 0, nil
	}
	if err != nil {
		m.failed(ctx, err)
		v = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return v + m.mem[day], nil
}

// prune keeps the newest memDaysKept days (yyyymmdd sorts by date). Caller
// holds mu.
func (m *CostMeter) prune() {
	if len(m.mem) <= memDaysKept {
		return
	}
	days := make([]string, 0, len(m.mem))
	for d := range m.mem {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days[:len(days)-memDaysKept] {
		delete(m.mem, d)
	}
}
