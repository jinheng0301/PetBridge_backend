package service

import (
	"testing"
	"time"
)

func TestLocalDayBounds(t *testing.T) {
	s := func(v string) *string { return &v }
	utc := func(v string) time.Time {
		tm, err := time.Parse(time.RFC3339, v)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		name       string
		now        time.Time
		tz         *string
		from, to   time.Time
		wantLength time.Duration
	}{
		{"seoul is already the next day", utc("2026-11-15T16:00:00Z"), s("Asia/Seoul"),
			utc("2026-11-15T15:00:00Z"), utc("2026-11-16T15:00:00Z"), 24 * time.Hour},
		{"new york fall back day is 25h", utc("2026-11-01T12:00:00Z"), s("America/New_York"),
			utc("2026-11-01T04:00:00Z"), utc("2026-11-02T05:00:00Z"), 25 * time.Hour},
		{"new york spring forward day is 23h", utc("2026-03-08T12:00:00Z"), s("America/New_York"),
			utc("2026-03-08T05:00:00Z"), utc("2026-03-09T04:00:00Z"), 23 * time.Hour},
		{"nil tz is UTC", utc("2026-11-15T16:00:00Z"), nil,
			utc("2026-11-15T00:00:00Z"), utc("2026-11-16T00:00:00Z"), 24 * time.Hour},
		{"empty tz is UTC", utc("2026-11-15T16:00:00Z"), s(""),
			utc("2026-11-15T00:00:00Z"), utc("2026-11-16T00:00:00Z"), 24 * time.Hour},
		{"unknown tz is UTC", utc("2026-11-15T16:00:00Z"), s("Not/AZone"),
			utc("2026-11-15T00:00:00Z"), utc("2026-11-16T00:00:00Z"), 24 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			from, to := LocalDayBounds(c.now, c.tz)
			if !from.Equal(c.from) || !to.Equal(c.to) {
				t.Fatalf("got [%v, %v) want [%v, %v)", from.UTC(), to.UTC(), c.from, c.to)
			}
			if to.Sub(from) != c.wantLength {
				t.Fatalf("length %v want %v", to.Sub(from), c.wantLength)
			}
		})
	}
}
