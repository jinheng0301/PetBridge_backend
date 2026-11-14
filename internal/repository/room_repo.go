package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

type RoomRepo struct{ pool *pgxpool.Pool }

func NewRoomRepo(pool *pgxpool.Pool) *RoomRepo { return &RoomRepo{pool: pool} }

// CreateForOwner creates the owner's own room with one member (the owner).
func (r *RoomRepo) CreateForOwner(ctx context.Context, ownerID string) (model.RoomInfo, error) {
	var room model.RoomInfo
	err := conn(ctx, r.pool).QueryRow(ctx,
		`insert into rooms(owner_id, member_count) values($1,1) returning id::text, member_count, capacity`,
		ownerID).Scan(&room.ID, &room.MemberCount, &room.Capacity)
	return room, err
}

func (r *RoomRepo) GetByOwner(ctx context.Context, ownerID string) (model.RoomInfo, error) {
	var room model.RoomInfo
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select id::text, member_count, capacity from rooms where owner_id=$1`, ownerID).
		Scan(&room.ID, &room.MemberCount, &room.Capacity)
	return room, err
}
