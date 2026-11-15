// Package ratelimit provides fixed-window counters (Redis, in-process, and a
// Redis-with-fallback combination) and the join-by-code guard built on them.
package ratelimit

import (
	"context"
	"time"
)

// Fixed values from the PRD/TRD; code constants, not configuration.
const (
	failureThreshold = 5
	failureWindow    = 10 * time.Minute
	cooldownTTL      = 30 * time.Minute
	ipLimit          = 30
	ipLimitDegraded  = 15 // used while Redis is unavailable
	ipWindow         = time.Hour
)

// Hit is the result of one Incr.
type Hit struct {
	Count    int64
	Resets   time.Duration // time left in the current window
	Degraded bool          // served by the in-process fallback, not Redis
}

// Counter is a fixed-window counter plus simple expiring flags.
type Counter interface {
	// Incr adds one to key. The window starts at the first hit and is not
	// extended by later ones.
	Incr(ctx context.Context, key string, window time.Duration) (Hit, error)
	SetFlag(ctx context.Context, key string, ttl time.Duration) error
	// FlagTTL returns the remaining time of a flag, 0 if it does not exist.
	FlagTTL(ctx context.Context, key string) (time.Duration, error)
	Delete(ctx context.Context, key string) error
}

// JoinGuard applies the room-join limits (design 5.3).
type JoinGuard struct{ c Counter }

func NewJoinGuard(c Counter) *JoinGuard { return &JoinGuard{c: c} }

func failKey(uid string) string     { return "rl:join:u:" + uid }
func cooldownKey(uid string) string { return "cd:join:u:" + uid }
func ipKey(ip string) string        { return "rl:join:ip:" + ip }

// Cooldown returns the time left in the user's cooldown; > 0 means cooling down.
func (g *JoinGuard) Cooldown(ctx context.Context, userID string) time.Duration {
	d, err := g.c.FlagTTL(ctx, cooldownKey(userID))
	if err != nil || d < 0 {
		return 0
	}
	return d
}

// Allow counts one attempt from ip. When denied, retryAfter is the time until
// the window resets.
func (g *JoinGuard) Allow(ctx context.Context, ip string) (ok bool, retryAfter time.Duration) {
	h, err := g.c.Incr(ctx, ipKey(ip), ipWindow)
	if err != nil {
		return true, 0 // fail open; Fallback never errors, so this is a bare Counter
	}
	limit := int64(ipLimit)
	if h.Degraded {
		limit = ipLimitDegraded
	}
	if h.Count > limit {
		return false, h.Resets
	}
	return true, 0
}

// RecordFailure counts one CODE_INVALID. The fifth within the window starts the
// cooldown and clears the counter.
func (g *JoinGuard) RecordFailure(ctx context.Context, userID string) {
	h, err := g.c.Incr(ctx, failKey(userID), failureWindow)
	if err != nil || h.Count < failureThreshold {
		return
	}
	if err := g.c.SetFlag(ctx, cooldownKey(userID), cooldownTTL); err != nil {
		return
	}
	_ = g.c.Delete(ctx, failKey(userID))
}

// RecordSuccess clears the user's failure counter.
func (g *JoinGuard) RecordSuccess(ctx context.Context, userID string) {
	_ = g.c.Delete(ctx, failKey(userID))
}
