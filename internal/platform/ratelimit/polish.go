package ratelimit

import (
	"context"
	"time"
)

// PolishGuard counts polish requests per user: a per-minute limit and a
// per-local-day quota (backend TRD 10.2). It implements service.PolishLimiter.
type PolishGuard struct {
	c                Counter
	perMinute, daily int
}

func NewPolishGuard(c Counter, perMinute, daily int) *PolishGuard {
	return &PolishGuard{c: c, perMinute: perMinute, daily: daily}
}

// Take counts one polish. The minute counter (rl:polish:u:{uid}) runs first;
// when it is over, the daily counter is left alone and the wait until the
// minute window resets is returned. The daily key is
// quota:polish:{uid}:{day} with dayLeft as its window. A counter error fails
// open (Fallback never returns one).
func (g *PolishGuard) Take(ctx context.Context, userID, day string, dayLeft time.Duration) (string, time.Duration) {
	if h, err := g.c.Incr(ctx, "rl:polish:u:"+userID, time.Minute); err == nil && h.Count > int64(g.perMinute) {
		return "minute_limit", h.Resets
	}
	if dayLeft <= 0 {
		dayLeft = time.Second
	}
	if h, err := g.c.Incr(ctx, "quota:polish:"+userID+":"+day, dayLeft); err == nil && h.Count > int64(g.daily) {
		return "daily_quota", 0
	}
	return "", 0
}
