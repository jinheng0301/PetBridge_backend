package ratelimit

import (
	"context"
	"time"
)

// Fallback uses primary and, when it errors, the long-lived secondary
// (in-process) counter. It never returns an error.
type Fallback struct {
	primary, secondary Counter
	onFallback         func(error)
}

// NewFallback builds a Fallback. onFallback (may be nil) is told why each call
// fell back, for logging. It is not called when the caller's own context has
// already ended: that failure says nothing about the primary's health.
func NewFallback(primary, secondary Counter, onFallback func(error)) *Fallback {
	return &Fallback{primary: primary, secondary: secondary, onFallback: onFallback}
}

// When the caller's context has already ended, a failing primary is not
// replaced by the secondary at all: the request is dead, and writing to the
// in-process counter would only leave keys there that the healthy Redis
// (which Cooldown keeps reading) never sees. Each method then returns its
// zero value and no error.

// failed reports err unless ctx has ended. The context, not the error, decides:
// a dial timeout to an unreachable Redis also satisfies
// errors.Is(err, context.DeadlineExceeded) while the caller is still waiting.
func (f *Fallback) failed(ctx context.Context, err error) {
	if f.onFallback != nil && ctx.Err() == nil {
		f.onFallback(err)
	}
}

func (f *Fallback) Incr(ctx context.Context, key string, window time.Duration) (Hit, error) {
	h, err := f.primary.Incr(ctx, key, window)
	if err == nil {
		return h, nil
	}
	f.failed(ctx, err)
	if ctx.Err() != nil {
		return Hit{Degraded: true}, nil
	}
	h, _ = f.secondary.Incr(ctx, key, window)
	h.Degraded = true
	return h, nil
}

func (f *Fallback) SetFlag(ctx context.Context, key string, ttl time.Duration) error {
	if err := f.primary.SetFlag(ctx, key, ttl); err != nil {
		f.failed(ctx, err)
		if ctx.Err() == nil {
			_ = f.secondary.SetFlag(ctx, key, ttl)
		}
	}
	return nil
}

func (f *Fallback) FlagTTL(ctx context.Context, key string) (time.Duration, error) {
	d, err := f.primary.FlagTTL(ctx, key)
	if err == nil {
		return d, nil
	}
	f.failed(ctx, err)
	if ctx.Err() != nil {
		return 0, nil
	}
	d, _ = f.secondary.FlagTTL(ctx, key)
	return d, nil
}

func (f *Fallback) Delete(ctx context.Context, key string) error {
	if err := f.primary.Delete(ctx, key); err != nil {
		f.failed(ctx, err)
		if ctx.Err() == nil {
			_ = f.secondary.Delete(ctx, key)
		}
	}
	return nil
}
