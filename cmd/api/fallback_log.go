package main

import (
	"log/slog"
	"sync"
	"time"
)

// newFallbackLogger returns the onFallback callback for ratelimit.Fallback.
// The callback fires on every rate-limit call while Redis is down, so it logs
// at most one warning per interval and reports how many it suppressed.
// It does not filter by error type: a dial timeout to an unreachable Redis
// satisfies errors.Is(err, context.DeadlineExceeded) too. Fallback itself
// skips the callback when the caller's own request context has ended.
func newFallbackLogger(log *slog.Logger, interval time.Duration, now func() time.Time) func(error) {
	var (
		mu         sync.Mutex
		last       time.Time
		suppressed int
	)
	return func(err error) {
		mu.Lock()
		t := now()
		if !last.IsZero() && t.Sub(last) < interval {
			suppressed++
			mu.Unlock()
			return
		}
		n := suppressed
		last, suppressed = t, 0
		mu.Unlock()
		log.Warn("redis unavailable, rate limits use in-process counters",
			"error", err.Error(), "suppressed", n)
	}
}
