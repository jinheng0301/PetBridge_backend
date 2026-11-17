package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/platform/logger"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
)

func TestFallbackLogger_ThrottlesWarnings(t *testing.T) {
	buf := &bytes.Buffer{}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	onFallback := newFallbackLogger(logger.New("debug", buf), "redis unavailable, rate limits use in-process counters", 30*time.Second, func() time.Time { return now })
	down := errors.New("dial tcp: connection refused")

	onFallback(down)
	onFallback(down)
	now = now.Add(29 * time.Second)
	onFallback(down)
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("lines within the interval = %d:\n%s", n, buf.String())
	}

	now = now.Add(time.Second)
	onFallback(down)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"suppressed":2`) ||
		!strings.Contains(lines[1], "connection refused") || !strings.Contains(lines[1], `"level":"WARN"`) {
		t.Fatalf("logs:\n%s", buf.String())
	}
}

// dialTimeoutCounter fails like go-redis dialling a blackholed host: the error
// satisfies errors.Is(err, context.DeadlineExceeded) although the caller's
// context is fine.
type dialTimeoutCounter struct{}

// errDialTimeout wraps a real dial timeout. The deadline has passed before the
// dial starts, so no packet is sent and it returns at once.
var errDialTimeout = func() error {
	_, err := (&net.Dialer{Timeout: time.Nanosecond}).Dial("tcp", "10.255.255.1:6379")
	return fmt.Errorf("redis incr: %w", err)
}()

func (dialTimeoutCounter) Incr(context.Context, string, time.Duration) (ratelimit.Hit, error) {
	return ratelimit.Hit{}, errDialTimeout
}
func (dialTimeoutCounter) SetFlag(context.Context, string, time.Duration) error { return errDialTimeout }
func (dialTimeoutCounter) FlagTTL(context.Context, string) (time.Duration, error) {
	return 0, errDialTimeout
}
func (dialTimeoutCounter) Delete(context.Context, string) error { return errDialTimeout }

// As wired in main: the caller's own cancelled or expired request is not
// logged as an outage, but a dial timeout with a live request is.
func TestFallbackLogging_CallerCancellationVersusDialTimeout(t *testing.T) {
	if !errors.Is(errDialTimeout, context.DeadlineExceeded) {
		t.Fatal("test premise: a dial timeout satisfies errors.Is(context.DeadlineExceeded)")
	}
	buf := &bytes.Buffer{}
	counter := ratelimit.NewFallback(dialTimeoutCounter{}, ratelimit.NewMemory(time.Now),
		newFallbackLogger(logger.New("debug", buf), "redis unavailable, rate limits use in-process counters", 30*time.Second, time.Now))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	for _, ctx := range []context.Context{cancelled, expired} {
		if h, err := counter.Incr(ctx, "k", time.Minute); err != nil || h != (ratelimit.Hit{Degraded: true}) {
			t.Fatalf("ended context Incr = %+v, %v", h, err)
		}
	}
	if buf.Len() != 0 {
		t.Fatalf("logged the caller's own cancellation:\n%s", buf.String())
	}

	// Skipped calls do not use up the interval: the outage is logged at once,
	// and the live call falls back to a secondary the dead calls never touched.
	h, err := counter.Incr(context.Background(), "k", time.Minute)
	if err != nil || !h.Degraded || h.Count != 1 {
		t.Fatalf("live Incr = %+v, %v; want degraded count 1", h, err)
	}
	if strings.Count(buf.String(), "\n") != 1 || !strings.Contains(buf.String(), "i/o timeout") {
		t.Fatalf("dial timeout with a live request not logged:\n%s", buf.String())
	}
}

func TestFallbackLogger_UsesGivenMessage(t *testing.T) {
	buf := &bytes.Buffer{}
	newFallbackLogger(logger.New("debug", buf), "redis unavailable, realtime events delivered locally", time.Minute, time.Now)(errors.New("down"))
	if !strings.Contains(buf.String(), `"msg":"redis unavailable, realtime events delivered locally"`) {
		t.Fatalf("log: %s", buf.String())
	}
}
