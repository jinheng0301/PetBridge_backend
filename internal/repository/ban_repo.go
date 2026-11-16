package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type BanRepo struct{ pool *pgxpool.Pool }

func NewBanRepo(pool *pgxpool.Pool) *BanRepo { return &BanRepo{pool: pool} }

// IsBanned reports whether the user is banned from the room. Malformed ids are never banned.
func (r *BanRepo) IsBanned(ctx context.Context, roomID, userID string) (bool, error) {
	if !uuidRE.MatchString(roomID) || !uuidRE.MatchString(userID) {
		return false, nil
	}
	var banned bool
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select exists(select 1 from room_bans where room_id=$1 and user_id=$2)`, roomID, userID).Scan(&banned)
	return banned, err
}

// Ban records that the owner removed the user from the room. Repeats and
// malformed ids are no-ops.
func (r *BanRepo) Ban(ctx context.Context, roomID, userID, bannedBy string) error {
	if !uuidRE.MatchString(roomID) || !uuidRE.MatchString(userID) {
		return nil
	}
	_, err := conn(ctx, r.pool).Exec(ctx, `insert into room_bans(room_id, user_id, banned_by) values($1,$2,$3)
		on conflict do nothing`, roomID, userID, bannedBy)
	return err
}
