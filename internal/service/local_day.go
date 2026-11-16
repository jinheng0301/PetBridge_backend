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
	return time.Date(y, m, d, 0, 0, 0, 0, loc), time.Date(y, m, d+1, 0, 0, 0, 0, loc)
}
