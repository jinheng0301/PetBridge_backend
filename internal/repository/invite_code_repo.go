package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type InviteCodeRepo struct{ pool *pgxpool.Pool }

func NewInviteCodeRepo(pool *pgxpool.Pool) *InviteCodeRepo { return &InviteCodeRepo{pool: pool} }

// InsertSignup stores a signup code. ON CONFLICT DO NOTHING is used because a
// failed INSERT would abort the surrounding transaction; inserted=false means
// the code already exists and the caller should try another one.
func (r *InviteCodeRepo) InsertSignup(ctx context.Context, code, roomID, userID string, expiresAt time.Time) (inserted bool, err error) {
	tag, err := conn(ctx, r.pool).Exec(ctx, `insert into invite_codes(code, room_id, issued_to, source, expires_at)
		values($1,$2,$3,'signup',$4) on conflict (code) do nothing`, code, roomID, userID, expiresAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FirstSignupFor returns the user's earliest signup code.
func (r *InviteCodeRepo) FirstSignupFor(ctx context.Context, userID string) (code string, expiresAt time.Time, err error) {
	err = conn(ctx, r.pool).QueryRow(ctx, `select code, expires_at from invite_codes where issued_to=$1 and source='signup'
		order by issued_at limit 1`, userID).Scan(&code, &expiresAt)
	return
}

// InsertZero stores a zero (seed) code; it is a no-op if the code exists.
func (r *InviteCodeRepo) InsertZero(ctx context.Context, code, roomID string, expiresAt time.Time) error {
	_, err := conn(ctx, r.pool).Exec(ctx, `insert into invite_codes(code, room_id, source, expires_at) values($1,$2,'zero',$3)
		on conflict (code) do nothing`, code, roomID, expiresAt)
	return err
}
