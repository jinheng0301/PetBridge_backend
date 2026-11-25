package repository

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

// CheckinRepo stores check-ins, room energy and unlocked decorations (B6b).
type CheckinRepo struct{ pool *pgxpool.Pool }

func NewCheckinRepo(pool *pgxpool.Pool) *CheckinRepo { return &CheckinRepo{pool: pool} }

// InsertCheckin records a check-in; inserted=false means the user already
// checked in to this room on localDay ("2006-01-02").
func (r *CheckinRepo) InsertCheckin(ctx context.Context, roomID, userID, localDay string, energy int, at time.Time) (bool, error) {
	tag, err := conn(ctx, r.pool).Exec(ctx, `insert into checkins(room_id, user_id, local_day, energy, created_at)
		values($1,$2,$3::date,$4,$5) on conflict do nothing`, roomID, userID, localDay, energy, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimFullHouse uses up one check-in of every seated active member, each
// member's oldest unused one newer than now-window, and reports whether it
// did. It does nothing unless there are at least two such members and all of
// them have one. The caller holds the room lock.
func (r *CheckinRepo) ClaimFullHouse(ctx context.Context, roomID string, now time.Time, window time.Duration) (bool, error) {
	tag, err := conn(ctx, r.pool).Exec(ctx, `
		with house as (
			select m.user_id from room_members m join users u on u.id = m.user_id
			where m.room_id = $1 and m.left_at is null and u.status = 'active'
		), pick as (
			select distinct on (c.user_id) c.user_id, c.local_day
			from checkins c join house h on h.user_id = c.user_id
			where c.room_id = $1 and c.bonus_at is null and c.created_at > $3
			order by c.user_id, c.created_at
		)
		update checkins c set bonus_at = $2
		from pick
		where c.room_id = $1 and c.user_id = pick.user_id and c.local_day = pick.local_day
		  and (select count(*) from house) >= 2
		  and (select count(*) from house) = (select count(*) from pick)`,
		roomID, now, now.Add(-window))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// AddEnergy adds delta to the room's energy and returns the new total.
func (r *CheckinRepo) AddEnergy(ctx context.Context, roomID string, delta int, at time.Time) (int, error) {
	var total int
	err := conn(ctx, r.pool).QueryRow(ctx, `insert into room_progress(room_id, energy_total, updated_at) values($1,$2,$3)
		on conflict (room_id) do update set energy_total = room_progress.energy_total + excluded.energy_total,
			updated_at = excluded.updated_at
		returning energy_total`, roomID, delta, at).Scan(&total)
	return total, err
}

// Energy returns the room's energy, 0 when nobody ever checked in.
func (r *CheckinRepo) Energy(ctx context.Context, roomID string) (int, error) {
	var total int
	err := conn(ctx, r.pool).QueryRow(ctx, `select energy_total from room_progress where room_id=$1`, roomID).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return total, err
}

// UnlockDecor stores the items the room does not have yet and returns those,
// in no particular order.
func (r *CheckinRepo) UnlockDecor(ctx context.Context, roomID string, itemIDs []string, at time.Time) ([]string, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}
	rows, err := conn(ctx, r.pool).Query(ctx, `insert into room_decor(room_id, item_id, unlocked_at)
		select $1, i, $3 from unnest($2::text[]) i
		on conflict do nothing returning item_id`, roomID, itemIDs, at)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ListDecor returns the room's unlocked decorations, oldest first; never nil.
func (r *CheckinRepo) ListDecor(ctx context.Context, roomID string) ([]model.DecorUnlock, error) {
	rows, err := conn(ctx, r.pool).Query(ctx,
		`select item_id, unlocked_at from room_decor where room_id=$1 order by unlocked_at, item_id`, roomID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[model.DecorUnlock])
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []model.DecorUnlock{}
	}
	for i := range out { // pgx scans into the server's zone
		out[i].UnlockedAt = out[i].UnlockedAt.UTC()
	}
	return out, nil
}

// MemberCheckins returns every seated member with their time zone and latest
// check-in day in this room.
func (r *CheckinRepo) MemberCheckins(ctx context.Context, roomID string) ([]model.MemberCheckin, error) {
	rows, err := conn(ctx, r.pool).Query(ctx, `select m.user_id::text, u.tz,
			(select max(c.local_day)::text from checkins c where c.room_id = m.room_id and c.user_id = m.user_id)
		from room_members m join users u on u.id = m.user_id
		where m.room_id = $1 and m.left_at is null
		order by m.seat_no`, roomID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[model.MemberCheckin])
}
