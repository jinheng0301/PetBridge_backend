package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

type RoomMemberRepo struct{ pool *pgxpool.Pool }

func NewRoomMemberRepo(pool *pgxpool.Pool) *RoomMemberRepo { return &RoomMemberRepo{pool: pool} }

var errNoFreeSeat = errors.New("repository: no free seat in room")

// AddOwner seats the owner at seat 1 of their own room.
func (r *RoomMemberRepo) AddOwner(ctx context.Context, roomID, userID string) error {
	_, err := conn(ctx, r.pool).Exec(ctx,
		`insert into room_members(room_id, user_id, seat_no, role) values($1,$2,1,'owner')`, roomID, userID)
	return err
}

// IsActiveMember reports whether the user currently sits in the room.
func (r *RoomMemberRepo) IsActiveMember(ctx context.Context, roomID, userID string) (bool, error) {
	if !uuidRE.MatchString(roomID) || !uuidRE.MatchString(userID) {
		return false, nil
	}
	var ok bool
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select exists(select 1 from room_members where room_id=$1 and user_id=$2 and left_at is null)`, roomID, userID).Scan(&ok)
	return ok, err
}

func (r *RoomMemberRepo) CountActiveForUser(ctx context.Context, userID string) (int, error) {
	var n int
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select count(*) from room_members where user_id=$1 and left_at is null`, userID).Scan(&n)
	return n, err
}

// AgeBands returns the age_band of every active member (members without one are skipped).
func (r *RoomMemberRepo) AgeBands(ctx context.Context, roomID string) ([]string, error) {
	rows, err := conn(ctx, r.pool).Query(ctx, `select u.age_band from room_members m join users u on u.id=m.user_id
		where m.room_id=$1 and m.left_at is null and u.age_band is not null order by m.seat_no`, roomID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// AddMember seats the user at the smallest free seat 1..5 and returns it.
// Callers hold the room lock; the unique index uq_seat_active is the backstop.
func (r *RoomMemberRepo) AddMember(ctx context.Context, roomID, userID string) (int, error) {
	var seat int
	err := conn(ctx, r.pool).QueryRow(ctx, `insert into room_members(room_id, user_id, seat_no, role)
		select $1, $2, s, 'member' from generate_series(1,5) s
		where not exists (select 1 from room_members where room_id=$1 and seat_no=s and left_at is null)
		order by s limit 1 returning seat_no`, roomID, userID).Scan(&seat)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errNoFreeSeat
	}
	return seat, err
}

// ListMembers returns the room's active members ordered by seat.
func (r *RoomMemberRepo) ListMembers(ctx context.Context, roomID string) ([]model.MemberInfo, error) {
	rows, err := conn(ctx, r.pool).Query(ctx, `select u.id::text, u.nickname, u.pet_type, m.seat_no, m.role
		from room_members m join users u on u.id=m.user_id
		where m.room_id=$1 and m.left_at is null order by m.seat_no`, roomID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.MemberInfo, error) {
		var m model.MemberInfo
		err := row.Scan(&m.UserID, &m.Nickname, &m.PetType, &m.SeatNo, &m.Role)
		return m, err
	})
}
