// Package queue is the asynq client that hands translation and polish work to the worker.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	goredis "github.com/redis/go-redis/v9"
)

const (
	TypeTranslate  = "translate"
	QueueTranslate = "translate"
	TypePolish     = "polish"
	QueuePolish    = "polish"
	maxRetry       = 2
)

// TranslatePayload is the task body. It carries only the id, never text.
type TranslatePayload struct {
	MessageID string `json:"message_id"`
}

// PolishPayload is the polish task body. It carries only the delivery id.
type PolishPayload struct {
	DeliveryID string `json:"delivery_id"`
}

// Queue enqueues tasks into Redis. It implements service.TranslateQueue and service.PolishQueue.
type Queue struct {
	c *asynq.Client
}

// New builds the queue on the process's shared Redis client, so enqueueing
// inherits its fail-fast timeouts and no-retry policy (TRD 12.2) instead of
// asynq's own slower defaults. The caller owns rdb and closes it.
func New(rdb *goredis.Client) *Queue {
	return &Queue{c: asynq.NewClientFromRedisClient(rdb)}
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

// EnqueuePolish adds polish:{deliveryID}[:dedupe]. A task with the same id
// still in the queue counts as success.
func (q *Queue) EnqueuePolish(ctx context.Context, deliveryID, dedupe string) error {
	payload, err := json.Marshal(PolishPayload{DeliveryID: deliveryID})
	if err != nil {
		return err
	}
	id := "polish:" + deliveryID
	if dedupe != "" {
		id += ":" + dedupe
	}
	_, err = q.c.EnqueueContext(ctx, asynq.NewTask(TypePolish, payload),
		asynq.TaskID(id), asynq.Queue(QueuePolish), asynq.MaxRetry(maxRetry))
	if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
		return fmt.Errorf("queue: enqueue polish: %w", err)
	}
	return nil
}

// ServerRedisOpt builds the asynq server's connection options from the
// process's go-redis client, so the worker reads exactly the address,
// credentials, DB and TLS the api's go-redis parsed from REDIS_URL. The server
// opens its own connections (blocking dequeues outlast rdb's 2s timeouts).
func ServerRedisOpt(rdb *goredis.Client) asynq.RedisClientOpt {
	o := rdb.Options()
	return asynq.RedisClientOpt{Network: o.Network, Addr: o.Addr, Username: o.Username, Password: o.Password,
		DB: o.DB, TLSConfig: o.TLSConfig}
}

// RetryDelay is the asynq RetryDelayFunc. n is the number of earlier retries
// (asynq passes msg.Retried: 0 for the first retry): 15s first, 60s after.
func RetryDelay(n int, _ error, _ *asynq.Task) time.Duration {
	if n <= 0 {
		return 15 * time.Second
	}
	return 60 * time.Second
}

// Close is a no-op: the Redis connection is shared and closed by its owner
// (asynq refuses to close a shared client).
func (q *Queue) Close() error { return nil }
