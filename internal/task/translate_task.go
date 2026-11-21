// Package task holds the asynq handlers run by cmd/worker.
package task

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/jinheng0301/petbridge/internal/platform/queue"
)

// NewTranslateHandler decodes the payload and calls process. A broken payload
// can never succeed, so it is not retried.
func NewTranslateHandler(process func(ctx context.Context, messageID string) error) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		var p queue.TranslatePayload
		if err := json.Unmarshal(t.Payload(), &p); err != nil || p.MessageID == "" {
			return fmt.Errorf("task: bad translate payload: %w", asynq.SkipRetry)
		}
		return process(ctx, p.MessageID)
	}
}
