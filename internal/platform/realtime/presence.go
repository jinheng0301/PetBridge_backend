package realtime

import (
	"context"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// PresenceStore keeps one sorted set per user: members are connection ids,
// scores are expiry times in Unix milliseconds. A user is online while any
// member has not expired, so several connections on several nodes count
// correctly and only the last one closing makes the user offline.
type PresenceStore struct {
	c      *goredis.Client
	prefix string
	ttl    time.Duration
}

// NewPresenceStore builds a store whose entries live for ttl unless refreshed.
func NewPresenceStore(c *goredis.Client, keyPrefix string, ttl time.Duration) *PresenceStore {
	return &PresenceStore{c: c, prefix: keyPrefix, ttl: ttl}
}

func (p *PresenceStore) key(userID string) string { return p.prefix + "presence:u:" + userID }

// Add records or refreshes connID until now+ttl, extends the key's TTL and
// returns the live count after the change, all in one MULTI.
func (p *PresenceStore) Add(ctx context.Context, userID, connID string, now time.Time) (int, error) {
	k := p.key(userID)
	return p.countAfter(ctx, k, now, func(pipe goredis.Pipeliner) {
		pipe.ZAdd(ctx, k, goredis.Z{Score: float64(now.Add(p.ttl).UnixMilli()), Member: connID})
		pipe.PExpire(ctx, k, p.ttl)
	})
}

// Remove drops connID and returns the live count after the change, in one MULTI.
func (p *PresenceStore) Remove(ctx context.Context, userID, connID string, now time.Time) (int, error) {
	k := p.key(userID)
	return p.countAfter(ctx, k, now, func(pipe goredis.Pipeliner) {
		pipe.ZRem(ctx, k, connID)
	})
}

// Count returns how many of the user's connections are still live at now,
// pruning expired ones on the way.
func (p *PresenceStore) Count(ctx context.Context, userID string, now time.Time) (int, error) {
	return p.countAfter(ctx, p.key(userID), now, func(goredis.Pipeliner) {})
}

// countAfter runs change, prunes expired members and counts live ones in a
// single MULTI, so concurrent connects and closes see a consistent count.
func (p *PresenceStore) countAfter(ctx context.Context, k string, now time.Time, change func(goredis.Pipeliner)) (int, error) {
	ms := strconv.FormatInt(now.UnixMilli(), 10)
	var count *goredis.IntCmd
	_, err := p.c.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		change(pipe)
		pipe.ZRemRangeByScore(ctx, k, "-inf", ms)
		count = pipe.ZCount(ctx, k, "("+ms, "+inf")
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(count.Val()), nil
}
