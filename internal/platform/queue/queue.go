// Package queue is the asynq client that hands translation work to the worker.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
)

const (
	TypeTranslate  = "translate"
	QueueTranslate = "translate"
	maxRetry       = 2
)

// TranslatePayload is the task body. It carries only the id, never text.
type TranslatePayload struct {
	MessageID string `json:"message_id"`
}

// Queue enqueues tasks into Redis. It implements service.TranslateQueue.
type Queue struct {
	c *asynq.Client
}

// New parses the Redis URL; only a malformed URL fails. The client connects lazily.
func New(redisURL string) (*Queue, error) {
	opt, err := asynq.ParseRedisURI(redisURL)
	if err != nil {
		return nil, errors.New("queue: invalid REDIS_URL")
	}
	return &Queue{c: asynq.NewClient(opt)}, nil
}

// EnqueueTranslate adds translate:{messageID}[:dedupe]. A task with the same id
// still in the queue counts as success.
func (q *Queue) EnqueueTranslate(ctx context.Context, messageID, dedupe string) error {
	payload, err := json.Marshal(TranslatePayload{MessageID: messageID})
	if err != nil {
		return err
	}
	id := "translate:" + messageID
	if dedupe != "" {
		id += ":" + dedupe
	}
	_, err = q.c.EnqueueContext(ctx, asynq.NewTask(TypeTranslate, payload),
		asynq.TaskID(id), asynq.Queue(QueueTranslate), asynq.MaxRetry(maxRetry))
	if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
		return fmt.Errorf("queue: enqueue translate: %w", err)
	}
	return nil
}

// RetryDelay is the asynq RetryDelayFunc. n is the number of earlier retries
// (asynq passes msg.Retried: 0 for the first retry): 15s first, 60s after.
func RetryDelay(n int, _ error, _ *asynq.Task) time.Duration {
	if n <= 0 {
		return 15 * time.Second
	}
	return 60 * time.Second
}

func (q *Queue) Close() error { return q.c.Close() }
