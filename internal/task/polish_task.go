package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/jinheng0301/petbridge/internal/platform/queue"
)

// NewPolishHandler decodes the payload and calls process. A broken payload can
// never succeed, so it is not retried.
func NewPolishHandler(process func(ctx context.Context, deliveryID string) error) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p queue.PolishPayload
		if err := json.Unmarshal(t.Payload(), &p); err != nil || p.DeliveryID == "" {
			return fmt.Errorf("task: bad polish payload: %w", asynq.SkipRetry)
		}
		return process(ctx, p.DeliveryID)
	}
}
