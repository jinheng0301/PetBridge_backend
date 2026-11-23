package repository

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
)

// ModFlagRepo writes the moderation review queue.
type ModFlagRepo struct{ pool *pgxpool.Pool }

func NewModFlagRepo(pool *pgxpool.Pool) *ModFlagRepo { return &ModFlagRepo{pool: pool} }

func (r *ModFlagRepo) Insert(ctx context.Context, f model.ModFlag) error {
	terms := f.Terms
	if terms == nil {
		terms = []string{}
	}
	_, err := conn(ctx, r.pool).Exec(ctx, `
		insert into mod_flags(stage, verdict, source, terms, user_id, room_id, message_id, delivery_id, target_lang, text)
		values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		f.Stage, f.Verdict, f.Source, terms, f.UserID, f.RoomID, f.MessageID, f.DeliveryID, f.TargetLang, f.Text)
	return err
}
