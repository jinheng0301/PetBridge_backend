package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
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

// InsertVoidZero stores a zero (seed) code that is already void; no-op if the code exists.
func (r *InviteCodeRepo) InsertVoidZero(ctx context.Context, code, roomID string, expiresAt time.Time) error {
	_, err := conn(ctx, r.pool).Exec(ctx, `insert into invite_codes(code, room_id, source, status, expires_at)
		values($1,$2,'zero','void',$3) on conflict (code) do nothing`, code, roomID, expiresAt)
	return err
}

// LockByCode loads a code with FOR UPDATE; an unknown code returns model.ErrNotFound.
func (r *InviteCodeRepo) LockByCode(ctx context.Context, code string) (model.InviteCode, error) {
	var c model.InviteCode
	err := conn(ctx, r.pool).QueryRow(ctx, `select code, room_id::text, issued_to::text, used_by::text, source, status, expires_at
		from invite_codes where code=$1 for update`, code).
		Scan(&c.Code, &c.RoomID, &c.IssuedTo, &c.UsedBy, &c.Source, &c.Status, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.InviteCode{}, model.ErrNotFound
	}
	return c, err
}

// RoomIDOf returns the code's room without locking; an unknown code returns model.ErrNotFound.
func (r *InviteCodeRepo) RoomIDOf(ctx context.Context, code string) (string, error) {
	var roomID string
	err := conn(ctx, r.pool).QueryRow(ctx, `select room_id::text from invite_codes where code=$1`, code).Scan(&roomID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", model.ErrNotFound
	}
	return roomID, err
}

// LockUnusedForRoom locks every unused code of the room FOR UPDATE in code
// order. Joiners lock the whole set before their own code and before the
// room, so one filling the room can void the others' codes without waiting
// on a transaction that in turn waits for the room.
func (r *InviteCodeRepo) LockUnusedForRoom(ctx context.Context, roomID string) error {
	rows, err := conn(ctx, r.pool).Query(ctx,
		`select code from invite_codes where room_id=$1 and status='unused' order by code for update`, roomID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

func (r *InviteCodeRepo) MarkUsed(ctx context.Context, code, userID string, at time.Time) error {
	_, err := conn(ctx, r.pool).Exec(ctx,
		`update invite_codes set status='used', used_by=$2, used_at=$3 where code=$1`, code, userID, at)
	return err
}

// VoidUnusedForRoom voids every unused code of the room and returns the
// distinct non-null holders (issued_to) of the voided codes.
func (r *InviteCodeRepo) VoidUnusedForRoom(ctx context.Context, roomID string) ([]string, error) {
	rows, err := conn(ctx, r.pool).Query(ctx, `with v as (
			update invite_codes set status='void' where room_id=$1 and status='unused' returning issued_to)
		select distinct issued_to::text from v where issued_to is not null order by 1`, roomID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// CountUnusedValid counts unused codes still valid at now (expires_at == now is expired).
func (r *InviteCodeRepo) CountUnusedValid(ctx context.Context, roomID string, now time.Time) (int, error) {
	var n int
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select count(*) from invite_codes where room_id=$1 and status='unused' and expires_at>$2`, roomID, now).Scan(&n)
	return n, err
}

// LifetimeCount counts codes ever issued to the user, excluding seeded zero codes.
func (r *InviteCodeRepo) LifetimeCount(ctx context.Context, userID string) (int, error) {
	var n int
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select count(*) from invite_codes where issued_to=$1 and source<>'zero'`, userID).Scan(&n)
	return n, err
}

// InsertReward stores a join reward code earned through forCode. inserted=false
// means the code already exists.
func (r *InviteCodeRepo) InsertReward(ctx context.Context, code, roomID, userID, forCode string, expiresAt time.Time) (bool, error) {
	tag, err := conn(ctx, r.pool).Exec(ctx, `insert into invite_codes(code, room_id, issued_to, source, expires_at, reward_for)
		values($1,$2,$3,'join_reward',$4,$5) on conflict (code) do nothing`, code, roomID, userID, expiresAt, forCode)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (r *InviteCodeRepo) RewardsFor(ctx context.Context, forCode string) ([]model.CodeInfo, error) {
	rows, err := conn(ctx, r.pool).Query(ctx,
		`select code, expires_at from invite_codes where reward_for=$1 order by issued_at, code`, forCode)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.CodeInfo, error) {
		var c model.CodeInfo
		err := row.Scan(&c.Code, &c.ExpiresAt)
		return c, err
	})
}

// ListForUser lists the user's codes newest first. An unused code past its
// expiry is reported as expired.
func (r *InviteCodeRepo) ListForUser(ctx context.Context, userID string, now time.Time) ([]model.CodeListItem, error) {
	rows, err := conn(ctx, r.pool).Query(ctx, `select c.code, c.source,
			case when c.status='unused' and c.expires_at<=$2 then 'expired' else c.status end,
			c.expires_at, rm.id::text, rm.member_count, rm.capacity
		from invite_codes c join rooms rm on rm.id=c.room_id
		where c.issued_to=$1 order by c.issued_at desc, c.code`, userID, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.CodeListItem, error) {
		var it model.CodeListItem
		err := row.Scan(&it.Code, &it.Source, &it.Status, &it.ExpiresAt, &it.Room.ID, &it.Room.MemberCount, &it.Room.Capacity)
		return it, err
	})
}
