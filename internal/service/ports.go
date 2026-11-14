package service

import (
	"context"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// The interfaces below are what services need from the repository layer.
// repository.* types satisfy them; unit tests may substitute fakes.

type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context) error) error
}

type UserStore interface {
	CreateEmpty(ctx context.Context) (string, error)
	State(ctx context.Context, userID string) (status string, onboarded bool, err error)
	LoadProfile(ctx context.Context, userID string, lock bool) (model.Profile, string, error)
	CompleteOnboarding(ctx context.Context, userID string, in model.OnboardingInput, pet string, at time.Time) error
}

type IdentityStore interface {
	FindUserID(ctx context.Context, provider, subject string) (string, error)
	Create(ctx context.Context, provider, subject, userID string) error
}

type RefreshTokenStore interface {
	Insert(ctx context.Context, userID, hash, deviceID string, expiresAt time.Time, family *string) (string, error)
	Rotate(ctx context.Context, hash string, now time.Time) (model.RotatedRefreshToken, error)
	StateByHash(ctx context.Context, hash string) (revoked bool, family string, err error)
	RevokeFamily(ctx context.Context, family string, now time.Time) error
	SetReplacedBy(ctx context.Context, oldID, newID string) error
	RevokeFamilyOfHash(ctx context.Context, hash string, now time.Time) error
}

type RoomStore interface {
	CreateForOwner(ctx context.Context, ownerID string) (model.RoomInfo, error)
	GetByOwner(ctx context.Context, ownerID string) (model.RoomInfo, error)
}

type RoomMemberStore interface {
	AddOwner(ctx context.Context, roomID, userID string) error
}

type InviteCodeStore interface {
	InsertSignup(ctx context.Context, code, roomID, userID string, expiresAt time.Time) (bool, error)
	FirstSignupFor(ctx context.Context, userID string) (code string, expiresAt time.Time, err error)
}
