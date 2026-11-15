package ratelimit

import (
	"context"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// INCR and the expiry run in one script so a crash between them cannot leave an
// immortal counter. The window starts at the first hit.
var incrScript = goredis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then redis.call('PEXPIRE', KEYS[1], ARGV[1]); ttl = tonumber(ARGV[1]) end
return {n, ttl}
`)

// Redis is a Counter backed by Redis. Build the client with the platform/redis
// package so calls fail within its 2 second timeouts.
type Redis struct{ c *goredis.Client }

func NewRedis(c *goredis.Client) *Redis { return &Redis{c: c} }

func (r *Redis) Incr(ctx context.Context, key string, window time.Duration) (Hit, error) {
	res, err := incrScript.Run(ctx, r.c, []string{key}, window.Milliseconds()).Int64Slice()
	if err != nil {
		return Hit{}, err
	}
	if len(res) != 2 {
		return Hit{}, errors.New("ratelimit: unexpected script reply")
	}
	return Hit{Count: res[0], Resets: time.Duration(res[1]) * time.Millisecond}, nil
}

func (r *Redis) SetFlag(ctx context.Context, key string, ttl time.Duration) error {
	return r.c.Set(ctx, key, 1, ttl).Err()
}

func (r *Redis) FlagTTL(ctx context.Context, key string) (time.Duration, error) {
	d, err := r.c.PTTL(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if d < 0 { // -2 missing, -1 no expiry
		return 0, nil
	}
	return d, nil
}

func (r *Redis) Delete(ctx context.Context, key string) error {
	return r.c.Del(ctx, key).Err()
}
