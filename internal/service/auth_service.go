package service

import (
	"context"
	"errors"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// AuthService implements login, refresh rotation and logout.
type AuthService struct {
	users      UserStore
	identities IdentityStore
	tokens     RefreshTokenStore
	tx         TxRunner
	issuer     *Issuer
	refreshTTL time.Duration
	now        func() time.Time // tests replace it with a fake clock
}

func NewAuthService(users UserStore, identities IdentityStore, tokens RefreshTokenStore, tx TxRunner,
	iss *Issuer, refreshTTL time.Duration) *AuthService {
	return &AuthService{users: users, identities: identities, tokens: tokens, tx: tx,
		issuer: iss, refreshTTL: refreshTTL, now: time.Now}
}

// LoginWithIdentity finds or creates the user behind (provider, subject) and
// starts a new refresh-token family. Login only creates a bare users row;
// profile, room and codes come from onboarding.
func (s *AuthService) LoginWithIdentity(ctx context.Context, provider, subject, deviceID string) (model.LoginResult, error) {
	userID, err := s.findOrCreateUser(ctx, provider, subject)
	if err != nil {
		return model.LoginResult{}, err
	}
	status, onboarded, err := s.users.State(ctx, userID)
	if err != nil {
		return model.LoginResult{}, err
	}
	if status != "active" {
		return model.LoginResult{}, ErrAccountDisabled
	}
	raw, _, err := s.insertRefresh(ctx, userID, deviceID, nil)
	if err != nil {
		return model.LoginResult{}, err
	}
	return s.result(userID, onboarded, raw)
}

// errLostRace aborts the creating transaction when another login created the
// same identity first.
var errLostRace = errors.New("identity created concurrently")

func (s *AuthService) findOrCreateUser(ctx context.Context, provider, subject string) (string, error) {
	id, err := s.identities.FindUserID(ctx, provider, subject)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, model.ErrNotFound) {
		return "", err
	}
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		uid, err := s.users.CreateEmpty(ctx)
		if err != nil {
			return err
		}
		if err := s.identities.Create(ctx, provider, subject, uid); err != nil {
			if errors.Is(err, model.ErrAlreadyExists) {
				return errLostRace
			}
			return err
		}
		id = uid
		return nil
	})
	if errors.Is(err, errLostRace) { // the empty user was rolled back; use the winner's user
		return s.identities.FindUserID(ctx, provider, subject)
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// insertRefresh stores a new refresh token (hash only). family nil starts a
// new family. It returns the raw token and the new row id.
func (s *AuthService) insertRefresh(ctx context.Context, userID, deviceID string, family *string) (raw, id string, err error) {
	raw, hash, err := NewRefreshToken()
	if err != nil {
		return "", "", err
	}
	id, err = s.tokens.Insert(ctx, userID, hash, deviceID, s.now().Add(s.refreshTTL), family)
	return raw, id, err
}

func (s *AuthService) result(userID string, onboarded bool, rawRefresh string) (model.LoginResult, error) {
	access, err := s.issuer.Sign(userID)
	if err != nil {
		return model.LoginResult{}, err
	}
	return model.LoginResult{
		Tokens:    model.Tokens{AccessToken: access, RefreshToken: rawRefresh, ExpiresIn: int(s.issuer.TTL().Seconds())},
		UserID:    userID,
		Onboarded: onboarded,
	}, nil
}

// Refresh rotates a refresh token. Presenting an already-revoked token is
// treated as theft: the whole family is revoked.
func (s *AuthService) Refresh(ctx context.Context, rawRefresh string) (model.LoginResult, error) {
	hash := HashRefreshToken(rawRefresh)
	now := s.now()

	var (
		userID, raw string
		onboarded   bool
		outcome     error // returned after a successful commit (the revocation must persist)
	)
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		old, err := s.tokens.Rotate(ctx, hash, now)
		if errors.Is(err, model.ErrNotFound) {
			outcome, err = s.refreshLoser(ctx, hash, now)
			return err
		}
		if err != nil {
			return err
		}

		status, ob, err := s.users.State(ctx, old.UserID)
		if err != nil {
			return err
		}
		if status != "active" {
			if err := s.tokens.RevokeFamily(ctx, old.FamilyID, now); err != nil {
				return err
			}
			outcome = ErrAccountDisabled
			return nil
		}

		dev := ""
		if old.DeviceID != nil {
			dev = *old.DeviceID
		}
		newRaw, newID, err := s.insertRefresh(ctx, old.UserID, dev, &old.FamilyID)
		if err != nil {
			return err
		}
		if err := s.tokens.SetReplacedBy(ctx, old.ID, newID); err != nil {
			return err
		}
		userID, onboarded, raw = old.UserID, ob, newRaw
		return nil
	})
	if err != nil {
		return model.LoginResult{}, err
	}
	if outcome != nil {
		return model.LoginResult{}, outcome
	}
	return s.result(userID, onboarded, raw)
}

// refreshLoser handles a token that could not be rotated. It returns the
// error to report; the second value is non-nil only when the transaction must
// roll back (nothing to persist).
func (s *AuthService) refreshLoser(ctx context.Context, hash string, now time.Time) (outcome, rollback error) {
	revoked, family, err := s.tokens.StateByHash(ctx, hash)
	if errors.Is(err, model.ErrNotFound) {
		return nil, ErrRefreshInvalid
	}
	if err != nil {
		return nil, err
	}
	if !revoked { // exists, not revoked: it must have expired
		return nil, ErrRefreshInvalid
	}
	if err := s.tokens.RevokeFamily(ctx, family, now); err != nil {
		return nil, err
	}
	return ErrRefreshReused, nil
}

// Logout revokes the whole family of the given refresh token. It is
// idempotent and succeeds for unknown tokens.
func (s *AuthService) Logout(ctx context.Context, rawRefresh string) error {
	return s.tokens.RevokeFamilyOfHash(ctx, HashRefreshToken(rawRefresh), s.now())
}

// EnsureUser finds or creates the user behind (provider, subject) without
// issuing any token. Used by the seed tool.
func (s *AuthService) EnsureUser(ctx context.Context, provider, subject string) (string, error) {
	return s.findOrCreateUser(ctx, provider, subject)
}
