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
	reuseGrace time.Duration
	now        func() time.Time // tests replace it with a fake clock
}

// RefreshReuseGrace is how long a just-rotated refresh token may be presented
// again without counting as theft (backend TRD 7.1, v1.8). It covers a client
// that never received or never stored the rotation's response: the server had
// already rotated, so the client still holds the previous token.
const RefreshReuseGrace = 60 * time.Second

func NewAuthService(users UserStore, identities IdentityStore, tokens RefreshTokenStore, tx TxRunner,
	iss *Issuer, refreshTTL time.Duration) *AuthService {
	return &AuthService{users: users, identities: identities, tokens: tokens, tx: tx,
		issuer: iss, refreshTTL: refreshTTL, reuseGrace: RefreshReuseGrace, now: time.Now}
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
// treated as theft and revokes the whole family, except within reuseGrace of
// its rotation while the family is still live: that is a client retrying a
// rotation it never received, and it gets a new pair, every other live token
// of the family being revoked so that only one stays usable.
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
			var retry bool
			old, retry, outcome, err = s.refreshLoser(ctx, hash, now)
			if err != nil || !retry {
				return err
			}
		} else if err != nil {
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

// refreshLoser handles a token that could not be rotated. retry reports a
// grace-window retry: the family's live tokens are already revoked and the
// caller issues a new one in old's place. Otherwise outcome is the error to
// report after commit; err is non-nil only when the transaction must roll back.
func (s *AuthService) refreshLoser(ctx context.Context, hash string, now time.Time) (
	old model.RotatedRefreshToken, retry bool, outcome, err error) {
	st, err := s.tokens.LockByHash(ctx, hash)
	if errors.Is(err, model.ErrNotFound) {
		return old, false, ErrRefreshInvalid, nil
	}
	if err != nil {
		return old, false, nil, err
	}
	if st.RevokedAt == nil { // exists, not revoked: it must have expired
		return old, false, ErrRefreshInvalid, nil
	}
	if st.ReplacedBy != nil && now.Sub(*st.RevokedAt) <= s.reuseGrace && st.ExpiresAt.After(now) {
		// Checked after the row lock, so a retry that committed meanwhile is seen.
		live, err := s.tokens.FamilyHasLive(ctx, st.FamilyID, now)
		if err != nil {
			return old, false, nil, err
		}
		if live {
			if err := s.tokens.RevokeFamily(ctx, st.FamilyID, now); err != nil {
				return old, false, nil, err
			}
			return st.RotatedRefreshToken, true, nil, nil
		}
	}
	if err := s.tokens.RevokeFamily(ctx, st.FamilyID, now); err != nil {
		return old, false, nil, err
	}
	return old, false, ErrRefreshReused, nil
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
