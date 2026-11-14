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

// StateByHash reports whether the token with this hash is revoked and which
// family it belongs to, or model.ErrNotFound.
func (r *RefreshTokenRepo) StateByHash(ctx context.Context, hash string) (revoked bool, family string, err error) {
	err = conn(ctx, r.pool).QueryRow(ctx,
		`select revoked_at is not null, family_id::text from refresh_tokens where token_hash=$1`, hash).Scan(&revoked, &family)
	if errors.Is(err, pgx.ErrNoRows) {
		err = model.ErrNotFound
	}
	return
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
