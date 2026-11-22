package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AIUsageRepo accumulates ai_usage_daily rows (service.UsageStore).
type AIUsageRepo struct{ pool *pgxpool.Pool }

func NewAIUsageRepo(pool *pgxpool.Pool) *AIUsageRepo { return &AIUsageRepo{pool: pool} }

// Add adds one batch of usage to (UTC date of day, userID, kind). calls == 0 writes nothing.
func (r *AIUsageRepo) Add(ctx context.Context, day time.Time, userID, kind string, calls int, in, out, costMicros int64) error {
	if calls == 0 {
		return nil
	}
	_, err := conn(ctx, r.pool).Exec(ctx, `
		insert into ai_usage_daily(day, user_id, kind, calls, tokens_in, tokens_out, cost_micro)
		values($1::date, $2, $3, $4, $5, $6, $7)
		on conflict (day, user_id, kind) do update set
			calls      = ai_usage_daily.calls + excluded.calls,
			tokens_in  = ai_usage_daily.tokens_in + excluded.tokens_in,
			tokens_out = ai_usage_daily.tokens_out + excluded.tokens_out,
			cost_micro = ai_usage_daily.cost_micro + excluded.cost_micro`,
		day.UTC().Format("2006-01-02"), userID, kind, calls, in, out, costMicros)
	return err
}
