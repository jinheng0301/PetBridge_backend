package ratelimit

import (
	"context"
	"sync"
	"time"
)

type memEntry struct {
	count   int64
	expires time.Time
}

// Memory is an in-process Counter. The clock is injected for tests.
type Memory struct {
	mu   sync.Mutex
	now  func() time.Time
	data map[string]memEntry
}

func NewMemory(now func() time.Time) *Memory {
	return &Memory{now: now, data: make(map[string]memEntry)}
}

func (m *Memory) live(key string, now time.Time) (memEntry, bool) {
	e, ok := m.data[key]
	if ok && !e.expires.After(now) {
		delete(m.data, key)
		return memEntry{}, false
	}
	return e, ok
}

func (m *Memory) Incr(_ context.Context, key string, window time.Duration) (Hit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	e, ok := m.live(key, now)
	if !ok {
		e = memEntry{expires: now.Add(window)}
	}
	e.count++
	m.data[key] = e
	return Hit{Count: e.count, Resets: e.expires.Sub(now)}, nil
}

func (m *Memory) SetFlag(_ context.Context, key string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = memEntry{count: 1, expires: m.now().Add(ttl)}
	return nil
}

func (m *Memory) FlagTTL(_ context.Context, key string) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	e, ok := m.live(key, now)
	if !ok {
		return 0, nil
	}
	return e.expires.Sub(now), nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}
