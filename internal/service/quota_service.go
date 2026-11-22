package service

import (
	"context"
	"errors"
	"time"
)

// Reasons in QUOTA_EXCEEDED details.reason (backend TRD 10.2, spec 5.7).
const (
	QuotaDailyQuota   = "daily_quota"
	QuotaMinuteLimit  = "minute_limit"
	QuotaGlobalBudget = "global_budget"
	QuotaDisabled     = "disabled"
)

var ErrQuotaExceeded = errors.New("polish quota exceeded")

// QuotaError says why a polish was refused; RetryAfter is set for minute_limit only.
type QuotaError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *QuotaError) Error() string        { return "polish quota exceeded: " + e.Reason }
func (e *QuotaError) Is(target error) bool { return target == ErrQuotaExceeded }

// PolishLimiter counts per-minute and per-day polishes; ratelimit.PolishGuard implements it.
type PolishLimiter interface {
	Take(ctx context.Context, userID, day string, dayLeft time.Duration) (reason string, retryAfter time.Duration)
}

type QuotaConfig struct {
	Enabled           bool  // AI_POLISH_ENABLED
	DailyBudgetMicros int64 // polishing stops once the UTC day's AI spend reaches this
}

// QuotaService decides whether one polish may be charged to a user.
type QuotaService struct {
	lim   PolishLimiter
	meter CostMeter
	cfg   QuotaConfig
	now   func() time.Time
}

func NewQuotaService(lim PolishLimiter, meter CostMeter, cfg QuotaConfig, now func() time.Time) *QuotaService {
	return &QuotaService{lim: lim, meter: meter, cfg: cfg, now: now}
}

// TakePolish checks, in order, the switch, the global budget, then the user's
// minute limit and daily quota (the day is the user's own, tz). It returns nil
// when one polish was charged, else a *QuotaError.
func (q *QuotaService) TakePolish(ctx context.Context, userID string, tz *string) error {
	if !q.cfg.Enabled {
		return &QuotaError{Reason: QuotaDisabled}
	}
	now := q.now()
	total, err := q.meter.Total(ctx, now.UTC().Format("20060102"))
	if err == nil && total >= q.cfg.DailyBudgetMicros {
		return &QuotaError{Reason: QuotaGlobalBudget}
	}
	from, to := LocalDayBounds(now, tz)
	if reason, wait := q.lim.Take(ctx, userID, from.Format("20060102"), to.Sub(now)); reason != "" {
		return &QuotaError{Reason: reason, RetryAfter: wait}
	}
	return nil
}
