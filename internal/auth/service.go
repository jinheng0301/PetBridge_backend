package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrAccountDisabled = errors.New("account disabled")
	ErrRefreshInvalid  = errors.New("refresh token invalid")
	ErrRefreshReused   = errors.New("refresh token reused")
)

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

// Service implements login, refresh rotation and logout.
type Service struct {
	pool       *pgxpool.Pool
	issuer     *Issuer
	refreshTTL time.Duration
	now        func() time.Time // tests replace it with a fake clock
}

func NewService(pool *pgxpool.Pool, iss *Issuer, refreshTTL time.Duration) *Service {
	return &Service{pool: pool, issuer: iss, refreshTTL: refreshTTL, now: time.Now}
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LoginWithIdentity finds or creates the user behind (provider, subject) and
// starts a new refresh-token family. Login only creates a bare users row;
// profile, room and codes come from onboarding.
func (s *Service) LoginWithIdentity(ctx context.Context, provider, subject, deviceID string) (LoginResult, error) {
	userID, err := s.findOrCreateUser(ctx, provider, subject)
	if err != nil {
		return LoginResult{}, err
	}
	status, onboarded, err := s.userState(ctx, s.pool, userID)
	if err != nil {
		return LoginResult{}, err
	}
	if status != "active" {
		return LoginResult{}, ErrAccountDisabled
	}
	raw, _, err := s.insertRefresh(ctx, s.pool, userID, deviceID, nil)
	if err != nil {
		return LoginResult{}, err
	}
	return s.result(userID, onboarded, raw)
}

func (s *Service) findOrCreateUser(ctx context.Context, provider, subject string) (string, error) {
	const find = `select user_id::text from auth_identities where provider=$1 and subject=$2`
	var id string
	err := s.pool.QueryRow(ctx, find, provider, subject).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := tx.QueryRow(ctx, `insert into users default values returning id::text`).Scan(&id); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `insert into auth_identities(provider, subject, user_id) values($1,$2,$3)`, provider, subject, id)
	if err != nil {
		if isUniqueViolation(err) { // lost the race: use the winner's user
			_ = tx.Rollback(ctx)
			if err := s.pool.QueryRow(ctx, find, provider, subject).Scan(&id); err != nil {
				return "", err
			}
			return id, nil
		}
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Service) userState(ctx context.Context, q querier, userID string) (status string, onboarded bool, err error) {
	err = q.QueryRow(ctx, `select status, onboarded_at is not null from users where id=$1`, userID).Scan(&status, &onboarded)
	return
}

// insertRefresh stores a new refresh token (hash only). family nil starts a
// new family. It returns the raw token and the new row id.
func (s *Service) insertRefresh(ctx context.Context, q querier, userID, deviceID string, family *string) (raw, id string, err error) {
	raw, hash, err := NewRefreshToken()
	if err != nil {
		return "", "", err
	}
	err = q.QueryRow(ctx, `insert into refresh_tokens(user_id, token_hash, device_id, expires_at, family_id)
		values($1,$2,$3,$4, coalesce($5::uuid, gen_random_uuid())) returning id::text`,
		userID, hash, nullIfEmpty(deviceID), s.now().Add(s.refreshTTL), family).Scan(&id)
	return raw, id, err
}

func (s *Service) result(userID string, onboarded bool, rawRefresh string) (LoginResult, error) {
	access, err := s.issuer.Sign(userID)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		Tokens:    Tokens{AccessToken: access, RefreshToken: rawRefresh, ExpiresIn: int(s.issuer.TTL().Seconds())},
		UserID:    userID,
		Onboarded: onboarded,
	}, nil
}

// Refresh rotates a refresh token. Presenting an already-revoked token is
// treated as theft: the whole family is revoked.
func (s *Service) Refresh(ctx context.Context, rawRefresh string) (LoginResult, error) {
	hash := HashRefreshToken(rawRefresh)
	now := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LoginResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Only one concurrent caller can flip revoked_at from NULL; the rest see 0 rows.
	var oldID, userID, family string
	var deviceID *string
	err = tx.QueryRow(ctx, `update refresh_tokens set revoked_at=$2
		where token_hash=$1 and revoked_at is null and expires_at > $2
		returning id::text, user_id::text, family_id::text, device_id`, hash, now).Scan(&oldID, &userID, &family, &deviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.refreshLoser(ctx, tx, hash, now)
	}
	if err != nil {
		return LoginResult{}, err
	}

	status, onboarded, err := s.userState(ctx, tx, userID)
	if err != nil {
		return LoginResult{}, err
	}
	if status != "active" {
		if err := revokeFamily(ctx, tx, family, now); err != nil {
			return LoginResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrAccountDisabled
	}

	dev := ""
	if deviceID != nil {
		dev = *deviceID
	}
	raw, newID, err := s.insertRefresh(ctx, tx, userID, dev, &family)
	if err != nil {
		return LoginResult{}, err
	}
	if _, err := tx.Exec(ctx, `update refresh_tokens set replaced_by=$2 where id=$1`, oldID, newID); err != nil {
		return LoginResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LoginResult{}, err
	}
	return s.result(userID, onboarded, raw)
}

func (s *Service) refreshLoser(ctx context.Context, tx pgx.Tx, hash string, now time.Time) (LoginResult, error) {
	var revoked bool
	var family string
	err := tx.QueryRow(ctx, `select revoked_at is not null, family_id::text from refresh_tokens where token_hash=$1`, hash).Scan(&revoked, &family)
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginResult{}, ErrRefreshInvalid
	}
	if err != nil {
		return LoginResult{}, err
	}
	if !revoked { // exists, not revoked: it must have expired
		return LoginResult{}, ErrRefreshInvalid
	}
	if err := revokeFamily(ctx, tx, family, now); err != nil {
		return LoginResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{}, ErrRefreshReused
}

func revokeFamily(ctx context.Context, tx pgx.Tx, family string, now time.Time) error {
	_, err := tx.Exec(ctx, `update refresh_tokens set revoked_at=$2 where family_id=$1 and revoked_at is null`, family, now)
	return err
}

// Logout revokes the whole family of the given refresh token. It is
// idempotent and succeeds for unknown tokens.
func (s *Service) Logout(ctx context.Context, rawRefresh string) error {
	_, err := s.pool.Exec(ctx, `update refresh_tokens set revoked_at=$2 where revoked_at is null
		and family_id = (select family_id from refresh_tokens where token_hash=$1)`,
		HashRefreshToken(rawRefresh), s.now())
	return err
}

// EnsureUser finds or creates the user behind (provider, subject) without
// issuing any token. Used by the seed tool.
func (s *Service) EnsureUser(ctx context.Context, provider, subject string) (string, error) {
	return s.findOrCreateUser(ctx, provider, subject)
}
