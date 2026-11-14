// Package redis wraps the Redis client.
package redis

import (
	"context"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const pingTimeout = 2 * time.Second

// Open parses the URL and pings. Errors never include the URL or password.
func Open(ctx context.Context, url string) (*goredis.Client, error) {
	opt, err := goredis.ParseURL(url)
	if err != nil {
		return nil, errors.New("redis: invalid REDIS_URL")
	}
	c := goredis.NewClient(opt)
	if err := Ping(ctx, c); err != nil {
		_ = c.Close()
		return nil, errors.New("redis: connection failed (" + opt.Addr + ")")
	}
	return c, nil
}

// Ping checks the client with a 2 second timeout.
func Ping(ctx context.Context, c *goredis.Client) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return c.Ping(ctx).Err()
}
