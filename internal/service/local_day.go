package service

import "time"

// LocalDayBounds returns the half-open interval [from, to) of the calendar day
// containing now in the IANA zone tz. A missing or unknown zone means UTC.
// The next midnight is built from calendar fields, not now+24h, so days with a
// daylight-saving shift are 23 or 25 hours long.
func LocalDayBounds(now time.Time, tz *string) (from, to time.Time) {
	loc := time.UTC
	if tz != nil && *tz != "" {
		if l, err := time.LoadLocation(*tz); err == nil {
			loc = l
		}
	}
	y, m, d := now.In(loc).Date()
	return startOfDay(y, m, d, loc), startOfDay(y, m, d+1, loc)
}

// startOfDay is the first instant of a calendar day in loc. Where clocks skip
// midnight (Havana in spring), time.Date may land on 23:00 of the day before;
// the day then starts at the zone change that follows.
func startOfDay(y int, m time.Month, d int, loc *time.Location) time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, loc)
	if t.Hour() == 0 || t.Day() == time.Date(y, m, d, 12, 0, 0, 0, loc).Day() {
		return t
	}
	_, end := t.ZoneBounds()
	return end
}
