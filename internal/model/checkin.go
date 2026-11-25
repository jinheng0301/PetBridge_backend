package model

import "time"

// DecorUnlock is one decoration a room has unlocked.
type DecorUnlock struct {
	ItemID     string    `json:"item_id"`
	UnlockedAt time.Time `json:"unlocked_at"`
}

// MemberCheckin is a seated member's time zone and the latest local day they
// checked in on ("2006-01-02"); LastDay is nil when they never did.
type MemberCheckin struct {
	UserID  string
	TZ      *string
	LastDay *string
}

// CheckinResult is the body of POST /rooms/{id}/checkin. Already marks a
// repeat on the same local day: nothing was gained and the rest is current.
type CheckinResult struct {
	Already          bool      `json:"already"`
	EnergyGained     int       `json:"energy_gained"`
	BonusGained      int       `json:"bonus_gained"`
	CheckinExpiresAt time.Time `json:"checkin_expires_at"`
	EnergyTotal      int       `json:"energy_total"`
	NextThreshold    *int      `json:"next_threshold"`
	Unlocked         []string  `json:"unlocked"`
}

// DecorListItem is one catalog item; UnlockedAt is null until the room has it.
type DecorListItem struct {
	ItemID     string     `json:"item_id"`
	Threshold  int        `json:"threshold"`
	UnlockedAt *time.Time `json:"unlocked_at"`
}

// DecorList is the body of GET /rooms/{id}/decor.
type DecorList struct {
	EnergyTotal   int             `json:"energy_total"`
	NextThreshold *int            `json:"next_threshold"`
	Items         []DecorListItem `json:"items"`
}
