package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type RewardRepo struct{ pool *pgxpool.Pool }

func NewRewardRepo(pool *pgxpool.Pool) *RewardRepo { return &RewardRepo{pool: pool} }

// Grant records that the user has received the join reward for this room.
// inserted=false means it was already granted.
func (r *RewardRepo) Grant(ctx context.Context, userID, roomID string, count int) (inserted bool, err error) {
	tag, err := conn(ctx, r.pool).Exec(ctx, `insert into reward_grants(user_id, room_id, granted_count)
		values($1,$2,$3) on conflict (user_id, room_id) do nothing`, userID, roomID, count)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// InsertEdge records who invited whom through which code.
func (r *RewardRepo) InsertEdge(ctx context.Context, inviterID, inviteeID, code string) error {
	_, err := conn(ctx, r.pool).Exec(ctx,
		`insert into invite_edges(inviter_id, invitee_id, code) values($1,$2,$3)`, inviterID, inviteeID, code)
	return err
}
