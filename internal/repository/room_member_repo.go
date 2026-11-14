package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

type RoomMemberRepo struct{ pool *pgxpool.Pool }

func NewRoomMemberRepo(pool *pgxpool.Pool) *RoomMemberRepo { return &RoomMemberRepo{pool: pool} }

// AddOwner seats the owner at seat 1 of their own room.
func (r *RoomMemberRepo) AddOwner(ctx context.Context, roomID, userID string) error {
	_, err := conn(ctx, r.pool).Exec(ctx,
		`insert into room_members(room_id, user_id, seat_no, role) values($1,$2,1,'owner')`, roomID, userID)
	return err
}
