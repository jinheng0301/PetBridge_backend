package service

import (
	"testing"
	"time"
)

func mustUTC(t *testing.T, v string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func strp(s string) *string { return &s }

func assertLocalDay(t *testing.T, now string, tz *string, wantDay, wantNext string) {
	t.Helper()
	day, next := LocalDay(mustUTC(t, now), tz)
	if day != wantDay || !next.Equal(mustUTC(t, wantNext)) || next.Location() != time.UTC {
		t.Fatalf("LocalDay = %s, %v; want %s, %s", day, next, wantDay, wantNext)
	}
}

func TestLocalDay(t *testing.T) {
	assertLocalDay(t, "2026-11-15T20:00:00Z", strp("Asia/Tokyo"), "2026-11-16", "2026-11-16T15:00:00Z")
	assertLocalDay(t, "2026-11-15T20:00:00Z", strp("America/Los_Angeles"), "2026-11-15", "2026-11-16T08:00:00Z")
}

func TestLocalDay_DST(t *testing.T) {
	assertLocalDay(t, "2026-11-01T12:00:00Z", strp("America/New_York"), "2026-11-01", "2026-11-02T05:00:00Z")
}

func TestLocalDay_BadZoneIsUTC(t *testing.T) {
	for _, tz := range []*string{nil, strp(""), strp("Nope/Zone")} {
		assertLocalDay(t, "2026-11-15T20:00:00Z", tz, "2026-11-15", "2026-11-16T00:00:00Z")
	}
}

func TestCheckinExpiry(t *testing.T) {
	now, tokyo := mustUTC(t, "2026-11-15T20:00:00Z"), strp("Asia/Tokyo")
	if got := CheckinExpiry(now, tokyo, strp("2026-11-16")); got == nil || !got.Equal(mustUTC(t, "2026-11-16T15:00:00Z")) {
		t.Fatalf("today: %v", got)
	}
	if got := CheckinExpiry(now, tokyo, strp("2026-11-15")); got != nil {
		t.Fatalf("yesterday: %v", got)
	}
	if got := CheckinExpiry(now, tokyo, nil); got != nil {
		t.Fatalf("never: %v", got)
	}
}

// Cairo and Havana spring forward at midnight, so 00:00 does not exist that
// day and time.Date may answer 23:00 of the day before.
func TestLocalDay_MissingMidnight(t *testing.T) {
	for _, c := range []struct{ tz, before, during, day string }{
		{"Africa/Cairo", "2026-04-23T12:00:00Z", "2026-04-24T10:00:00Z", "2026-04-24"},
		{"America/Havana", "2026-03-07T12:00:00Z", "2026-03-08T12:00:00Z", "2026-03-08"},
	} {
		if day, _ := LocalDay(mustUTC(t, c.during), &c.tz); day != c.day {
			t.Errorf("%s: LocalDay = %s want %s", c.tz, day, c.day)
		}
		day, next := LocalDay(mustUTC(t, c.before), &c.tz)
		if after, _ := LocalDay(next, &c.tz); after != c.day {
			t.Errorf("%s: day %s ends at %v, which is still %s", c.tz, day, next, after)
		}
	}
}
