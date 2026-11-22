package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestPolishGuard(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	g := NewPolishGuard(NewMemory(clk.now), 6, 100)
	for i := 0; i < 6; i++ {
		if r, _ := g.Take(ctx, "u1", "20261115", 12*time.Hour); r != "" {
			t.Fatalf("take %d: %q", i+1, r)
		}
	}
	r, wait := g.Take(ctx, "u1", "20261115", 12*time.Hour)
	if r != "minute_limit" || wait <= 0 || wait > time.Minute {
		t.Fatalf("7th: %q %v", r, wait)
	}
	if r, _ := g.Take(ctx, "u2", "20261115", 12*time.Hour); r != "" {
		t.Fatalf("other user limited: %q", r)
	}
	clk.advance(time.Minute)
	if r, _ := g.Take(ctx, "u1", "20261115", 12*time.Hour); r != "" {
		t.Fatalf("after a minute: %q", r)
	}
}

func TestPolishGuard_DailyAndMinuteDenialNotCounted(t *testing.T) {
	ctx := context.Background()
	clk := newFakeClock()
	g := NewPolishGuard(NewMemory(clk.now), 1, 3)
	if r, _ := g.Take(ctx, "u", "d1", 12*time.Hour); r != "" {
		t.Fatal(r)
	}
	if r, _ := g.Take(ctx, "u", "d1", 12*time.Hour); r != "minute_limit" { // must not use up a daily slot
		t.Fatal(r)
	}
	for i := 0; i < 2; i++ {
		clk.advance(time.Minute)
		if r, _ := g.Take(ctx, "u", "d1", 12*time.Hour); r != "" {
			t.Fatalf("daily slot %d: %q", i+2, r)
		}
	}
	clk.advance(time.Minute)
	r, wait := g.Take(ctx, "u", "d1", 12*time.Hour)
	if r != "daily_quota" || wait != 0 {
		t.Fatalf("4th of the day: %q %v", r, wait)
	}
	clk.advance(time.Minute)
	if r, _ := g.Take(ctx, "u", "d2", 12*time.Hour); r != "" { // next local day has its own key
		t.Fatalf("next day: %q", r)
	}
}

func TestPolishGuard_ZeroDaily(t *testing.T) {
	g := NewPolishGuard(NewMemory(newFakeClock().now), 6, 0)
	if r, _ := g.Take(context.Background(), "u", "d", time.Hour); r != "daily_quota" {
		t.Fatalf("got %q", r)
	}
}
