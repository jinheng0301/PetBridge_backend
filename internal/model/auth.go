package model

import "time"

type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int // seconds
}

type LoginResult struct {
	Tokens
	UserID    string
	Onboarded bool
}

// RotatedRefreshToken describes the refresh-token row that was just revoked
// by a rotation.
type RotatedRefreshToken struct {
	ID       string
	UserID   string
	FamilyID string
	DeviceID *string
}

// RefreshTokenState is a refresh-token row as read (and locked) when it
// could not be rotated: it is unknown, expired, or already revoked.
type RefreshTokenState struct {
	RotatedRefreshToken
	RevokedAt  *time.Time
	ReplacedBy *string
	ExpiresAt  time.Time
}
