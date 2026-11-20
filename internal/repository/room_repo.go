package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

type RoomRepo struct{ pool *pgxpool.Pool }

func NewRoomRepo(pool *pgxpool.Pool) *RoomRepo { return &RoomRepo{pool: pool} }

// CreateForOwner creates the owner's own room with one member (the owner).
func (r *RoomRepo) CreateForOwner(ctx context.Context, ownerID string, capacity int) (model.RoomInfo, error) {
	var room model.RoomInfo
	err := conn(ctx, r.pool).QueryRow(ctx,
		`insert into rooms(owner_id, member_count, capacity) values($1,1,$2) returning id::text, member_count, capacity`,
		ownerID, capacity).Scan(&room.ID, &room.MemberCount, &room.Capacity)
	return room, err
}

func (r *RoomRepo) GetByOwner(ctx context.Context, ownerID string) (model.RoomInfo, error) {
	var room model.RoomInfo
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select id::text, member_count, capacity from rooms where owner_id=$1`, ownerID).
		Scan(&room.ID, &room.MemberCount, &room.Capacity)
	return room, err
}

// LockRooms locks the given rooms FOR UPDATE in id order. Ids are deduplicated,
// so callers locking the same pair in opposite order cannot deadlock. Missing
// or malformed ids are absent from the result.
func (r *RoomRepo) LockRooms(ctx context.Context, ids ...string) (map[string]model.RoomState, error) {
	seen := map[string]bool{}
	var uniq []string
	for _, id := range ids {
		id = strings.ToLower(id)
		if uuidRE.MatchString(id) && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	out := map[string]model.RoomState{}
	if len(uniq) == 0 {
		return out, nil
	}
	rows, err := conn(ctx, r.pool).Query(ctx, `select id::text, owner_id::text, member_count, capacity, status
		from rooms where id = any($1::uuid[]) order by id for update`, uniq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s model.RoomState
		if err := rows.Scan(&s.ID, &s.OwnerID, &s.MemberCount, &s.Capacity, &s.Status); err != nil {
			return nil, err
		}
		out[s.ID] = s
	}
	return out, rows.Err()
}

// Admit increments member_count if the room is open and has space. ok=false
// means the room is full or not open.
func (r *RoomRepo) Admit(ctx context.Context, roomID string) (memberCount int, ok bool, err error) {
	err = conn(ctx, r.pool).QueryRow(ctx, `update rooms set member_count=member_count+1
		where id=$1 and status='open' and member_count<capacity returning member_count`, roomID).Scan(&memberCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return memberCount, true, nil
}

func (r *RoomRepo) MarkFull(ctx context.Context, roomID string) error {
	_, err := conn(ctx, r.pool).Exec(ctx, `update rooms set status='full' where id=$1`, roomID)
	return err
}

// ListForMember returns the rooms the user currently sits in (own room first,
// then by join time) with their active members filled in.
func (r *RoomRepo) ListForMember(ctx context.Context, userID string) ([]model.RoomDetail, error) {
	rows, err := conn(ctx, r.pool).Query(ctx, `select rm.id::text, rm.status, rm.member_count, rm.capacity, rm.owner_id=$1
		from rooms rm join room_members m on m.room_id=rm.id and m.user_id=$1 and m.left_at is null
		order by (rm.owner_id=$1) desc, m.joined_at, rm.id`, userID)
	if err != nil {
		return nil, err
	}
	details, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.RoomDetail, error) {
		var d model.RoomDetail
		err := row.Scan(&d.ID, &d.Status, &d.MemberCount, &d.Capacity, &d.IsOwner)
		return d, err
	})
	if err != nil || len(details) == 0 {
		return details, err
	}
	ids := make([]string, len(details))
	idx := map[string]int{}
	for i, d := range details {
		ids[i] = d.ID
		idx[d.ID] = i
		details[i].Members = []model.MemberInfo{}
	}
	mrows, err := conn(ctx, r.pool).Query(ctx, `select m.room_id::text, u.id::text, u.nickname, u.pet_type, m.seat_no, m.role
		from room_members m join users u on u.id=m.user_id
		where m.room_id = any($1::uuid[]) and m.left_at is null order by m.room_id, m.seat_no`, ids)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var roomID string
		var m model.MemberInfo
		if err := mrows.Scan(&roomID, &m.UserID, &m.Nickname, &m.PetType, &m.SeatNo, &m.Role); err != nil {
			return nil, err
		}
		details[idx[roomID]].Members = append(details[idx[roomID]].Members, m)
	}
	return details, mrows.Err()
}

// Peek reads a room without locking it. An unknown or malformed id returns
// model.ErrNotFound.
func (r *RoomRepo) Peek(ctx context.Context, roomID string) (model.RoomState, error) {
	if !uuidRE.MatchString(strings.ToLower(roomID)) {
		return model.RoomState{}, model.ErrNotFound
	}
	var s model.RoomState
	err := conn(ctx, r.pool).QueryRow(ctx, `select id::text, owner_id::text, member_count, capacity, status
		from rooms where id=$1`, roomID).Scan(&s.ID, &s.OwnerID, &s.MemberCount, &s.Capacity, &s.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.RoomState{}, model.ErrNotFound
	}
	return s, err
}

// Vacate frees one seat: member_count-1, and a full room reopens. The caller
// holds the room lock.
func (r *RoomRepo) Vacate(ctx context.Context, roomID string) (int, error) {
	var n int
	err := conn(ctx, r.pool).QueryRow(ctx, `update rooms set member_count=member_count-1,
		status=case when status='full' then 'open' else status end where id=$1 returning member_count`, roomID).Scan(&n)
	return n, err
}

// Reset returns a dissolved room to its initial state: only the owner, open.
func (r *RoomRepo) Reset(ctx context.Context, roomID string) error {
	_, err := conn(ctx, r.pool).Exec(ctx, `update rooms set member_count=1, status='open' where id=$1`, roomID)
	return err
}
