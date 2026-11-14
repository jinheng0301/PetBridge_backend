package model

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
