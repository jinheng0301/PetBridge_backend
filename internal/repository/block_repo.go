package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

// BlockRepo stores user blocks.
type BlockRepo struct{ pool *pgxpool.Pool }

func NewBlockRepo(pool *pgxpool.Pool) *BlockRepo { return &BlockRepo{pool: pool} }

// Block does nothing when the block already exists.
func (r *BlockRepo) Block(ctx context.Context, blockerID, blockedID string) error {
	_, err := conn(ctx, r.pool).Exec(ctx,
		`insert into blocks(blocker_id, blocked_id) values($1,$2) on conflict do nothing`, blockerID, blockedID)
	return err
}

func (r *BlockRepo) Unblock(ctx context.Context, blockerID, blockedID string) error {
	if !uuidRE.MatchString(blockerID) || !uuidRE.MatchString(blockedID) {
		return nil
	}
	_, err := conn(ctx, r.pool).Exec(ctx, `delete from blocks where blocker_id=$1 and blocked_id=$2`, blockerID, blockedID)
	return err
}

func (r *BlockRepo) IsBlocking(ctx context.Context, blockerID, blockedID string) (bool, error) {
	if !uuidRE.MatchString(blockerID) || !uuidRE.MatchString(blockedID) {
		return false, nil
	}
	var ok bool
	err := conn(ctx, r.pool).QueryRow(ctx,
		`select exists(select 1 from blocks where blocker_id=$1 and blocked_id=$2)`, blockerID, blockedID).Scan(&ok)
	return ok, err
}

func (r *BlockRepo) Count(ctx context.Context, blockerID string) (int, error) {
	if !uuidRE.MatchString(blockerID) {
		return 0, nil
	}
	var n int
	err := conn(ctx, r.pool).QueryRow(ctx, `select count(*) from blocks where blocker_id=$1`, blockerID).Scan(&n)
	return n, err
}

// List returns the blocker's list, newest first.
func (r *BlockRepo) List(ctx context.Context, blockerID string) ([]model.BlockedUser, error) {
	out := []model.BlockedUser{}
	if !uuidRE.MatchString(blockerID) {
		return out, nil
	}
	rows, err := conn(ctx, r.pool).Query(ctx, `
		select b.blocked_id::text, u.nickname, u.pet_type, b.created_at
		from blocks b join users u on u.id = b.blocked_id
		where b.blocker_id = $1
		order by b.created_at desc, b.blocked_id`, blockerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b model.BlockedUser
		if err := rows.Scan(&b.UserID, &b.Nickname, &b.PetType, &b.BlockedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Between reports whether either user blocks the other.
func (r *BlockRepo) Between(ctx context.Context, a, b string) (bool, error) {
	if !uuidRE.MatchString(a) || !uuidRE.MatchString(b) {
		return false, nil
	}
	var ok bool
	err := conn(ctx, r.pool).QueryRow(ctx, `
		select exists(select 1 from blocks
		where (blocker_id=$1 and blocked_id=$2) or (blocker_id=$2 and blocked_id=$1))`, a, b).Scan(&ok)
	return ok, err
}

// BlockedAmong returns the ids in others that have a block with userID in either direction.
func (r *BlockRepo) BlockedAmong(ctx context.Context, userID string, others []string) (map[string]bool, error) {
	out := map[string]bool{}
	if !uuidRE.MatchString(userID) {
		return out, nil
	}
	valid := make([]string, 0, len(others))
	for _, o := range others {
		if uuidRE.MatchString(o) {
			valid = append(valid, o)
		}
	}
	if len(valid) == 0 {
		return out, nil
	}
	rows, err := conn(ctx, r.pool).Query(ctx, `
		select blocked_id::text from blocks where blocker_id = $1 and blocked_id = any($2::uuid[])
		union
		select blocker_id::text from blocks where blocked_id = $1 and blocker_id = any($2::uuid[])`, userID, valid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
