package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recLimiter struct {
	calls   int
	day     string
	dayLeft time.Duration
	reason  string
	retry   time.Duration
}

func (l *recLimiter) Take(_ context.Context, _ string, day string, dayLeft time.Duration) (string, time.Duration) {
	l.calls++
	l.day, l.dayLeft = day, dayLeft
	return l.reason, l.retry
}

func TestQuotaService_Order(t *testing.T) {
	now := func() time.Time { return gwNow }
	ctx := context.Background()

	lim := &recLimiter{}
	q := NewQuotaService(lim, newGWMeter(), QuotaConfig{Enabled: false, DailyBudgetMicros: 100}, now)
	var qe *QuotaError
	if err := q.TakePolish(ctx, "u", nil); !errors.As(err, &qe) || qe.Reason != QuotaDisabled || lim.calls != 0 {
		t.Fatalf("disabled: %v calls=%d", err, lim.calls)
	}

	meter := newGWMeter()
	meter.data[gwDay] = 100
	q = NewQuotaService(lim, meter, QuotaConfig{Enabled: true, DailyBudgetMicros: 100}, now)
	if err := q.TakePolish(ctx, "u", nil); !errors.As(err, &qe) || qe.Reason != QuotaGlobalBudget || lim.calls != 0 {
		t.Fatalf("budget: %v calls=%d", err, lim.calls)
	}

	meter.data[gwDay] = 99
	if err := q.TakePolish(ctx, "u", nil); err != nil || lim.calls != 1 {
		t.Fatalf("ok: %v calls=%d", err, lim.calls)
	}
	lim.reason, lim.retry = QuotaMinuteLimit, 30*time.Second
	err := q.TakePolish(ctx, "u", nil)
	if !errors.Is(err, ErrQuotaExceeded) || !errors.As(err, &qe) || qe.Reason != QuotaMinuteLimit || qe.RetryAfter != 30*time.Second {
		t.Fatalf("minute: %v", err)
	}
}

func TestQuotaService_UsesUserDay(t *testing.T) {
	lim := &recLimiter{}
	at := time.Date(2026, 11, 22, 15, 30, 0, 0, time.UTC)
	q := NewQuotaService(lim, newGWMeter(), QuotaConfig{Enabled: true, DailyBudgetMicros: 100}, func() time.Time { return at })
	tokyo := "Asia/Tokyo"
	if err := q.TakePolish(context.Background(), "u", &tokyo); err != nil {
		t.Fatal(err)
	}
	if lim.day != "20261123" || lim.dayLeft != 23*time.Hour+30*time.Minute {
		t.Fatalf("tokyo day=%s left=%v", lim.day, lim.dayLeft)
	}
	if err := q.TakePolish(context.Background(), "u", nil); err != nil {
		t.Fatal(err)
	}
	if lim.day != "20261122" || lim.dayLeft != 8*time.Hour+30*time.Minute {
		t.Fatalf("utc day=%s left=%v", lim.day, lim.dayLeft)
	}
}
