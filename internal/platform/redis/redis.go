// Package redis wraps the Redis client.
package redis

import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const pingTimeout = 2 * time.Second

// New parses the URL and builds a client without connecting. go-redis dials
// lazily and reconnects by itself, so this is also the client to keep when
// Redis is down at startup. The error never includes the URL or password.
func New(url string) (*goredis.Client, error) {
	opt, err := goredis.ParseURL(url)
	if err != nil {
		return nil, errors.New("redis: invalid REDIS_URL")
	}
	// Redis is a degradable dependency (TRD 12.2): fail fast instead of letting a
	// hung or refusing server stall callers. go-redis's own defaults (3s reads,
	// 3 retries) also bypass the context deadline during connection setup.
	opt.DialTimeout = pingTimeout
	opt.ReadTimeout = pingTimeout
	opt.WriteTimeout = pingTimeout
	opt.MaxRetries = -1 // -1 disables retries
	return goredis.NewClient(opt), nil
}

// Open is New plus a ping. The ping error is wrapped, not dropped, so logs can
// tell a refused connection from WRONGPASS or a TLS failure; go-redis errors
// carry the address and server message, never the password.
func Open(ctx context.Context, url string) (*goredis.Client, error) {
	c, err := New(url)
	if err != nil {
		return nil, err
	}
	if err := Ping(ctx, c); err != nil {
		addr := c.Options().Addr
		_ = c.Close()
		return nil, fmt.Errorf("redis: connection failed (%s): %w", addr, err)
	}
	return c, nil
}

// Ping checks the client with a 2 second timeout.
func Ping(ctx context.Context, c *goredis.Client) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return c.Ping(ctx).Err()
}
