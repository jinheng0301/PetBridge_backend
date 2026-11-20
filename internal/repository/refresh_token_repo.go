package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

type RefreshTokenRepo struct{ pool *pgxpool.Pool }

func NewRefreshTokenRepo(pool *pgxpool.Pool) *RefreshTokenRepo { return &RefreshTokenRepo{pool: pool} }

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Insert stores a refresh token (hash only). family nil starts a new family.
// It returns the new row id.
func (r *RefreshTokenRepo) Insert(ctx context.Context, userID, hash, deviceID string, expiresAt time.Time, family *string) (string, error) {
	var id string
	err := conn(ctx, r.pool).QueryRow(ctx, `insert into refresh_tokens(user_id, token_hash, device_id, expires_at, family_id)
		values($1,$2,$3,$4, coalesce($5::uuid, gen_random_uuid())) returning id::text`,
		userID, hash, nullIfEmpty(deviceID), expiresAt, family).Scan(&id)
	return id, err
}

// Rotate revokes the live token with this hash. Only one concurrent caller can
// flip revoked_at from NULL; the rest get model.ErrNotFound.
func (r *RefreshTokenRepo) Rotate(ctx context.Context, hash string, now time.Time) (model.RotatedRefreshToken, error) {
	var t model.RotatedRefreshToken
	err := conn(ctx, r.pool).QueryRow(ctx, `update refresh_tokens set revoked_at=$2
		where token_hash=$1 and revoked_at is null and expires_at > $2
		returning id::text, user_id::text, family_id::text, device_id`, hash, now).
		Scan(&t.ID, &t.UserID, &t.FamilyID, &t.DeviceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.RotatedRefreshToken{}, model.ErrNotFound
	}
	return t, err
}

// LockByHash loads the token with this hash FOR UPDATE, or model.ErrNotFound.
// The lock serialises concurrent retries of the same rotated token.
func (r *RefreshTokenRepo) LockByHash(ctx context.Context, hash string) (model.RefreshTokenState, error) {
	var t model.RefreshTokenState
	err := conn(ctx, r.pool).QueryRow(ctx, `select id::text, user_id::text, family_id::text, device_id,
			revoked_at, replaced_by::text, expires_at
		from refresh_tokens where token_hash=$1 for update`, hash).
		Scan(&t.ID, &t.UserID, &t.FamilyID, &t.DeviceID, &t.RevokedAt, &t.ReplacedBy, &t.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.RefreshTokenState{}, model.ErrNotFound
	}
	return t, err
}

// FamilyHasLive reports whether the family still holds a usable token
// (not revoked, not expired at now). Logout and theft revocation leave none.
func (r *RefreshTokenRepo) FamilyHasLive(ctx context.Context, family string, now time.Time) (bool, error) {
	var ok bool
	err := conn(ctx, r.pool).QueryRow(ctx, `select exists(select 1 from refresh_tokens
		where family_id=$1 and revoked_at is null and expires_at > $2)`, family, now).Scan(&ok)
	return ok, err
}

func (r *RefreshTokenRepo) RevokeFamily(ctx context.Context, family string, now time.Time) error {
	_, err := conn(ctx, r.pool).Exec(ctx,
		`update refresh_tokens set revoked_at=$2 where family_id=$1 and revoked_at is null`, family, now)
	return err
}

func (r *RefreshTokenRepo) SetReplacedBy(ctx context.Context, oldID, newID string) error {
	_, err := conn(ctx, r.pool).Exec(ctx, `update refresh_tokens set replaced_by=$2 where id=$1`, oldID, newID)
	return err
}

// RevokeFamilyOfHash revokes the whole family of the token with this hash.
// It succeeds for unknown tokens.
func (r *RefreshTokenRepo) RevokeFamilyOfHash(ctx context.Context, hash string, now time.Time) error {
	_, err := conn(ctx, r.pool).Exec(ctx, `update refresh_tokens set revoked_at=$2 where revoked_at is null
		and family_id = (select family_id from refresh_tokens where token_hash=$1)`, hash, now)
	return err
}
