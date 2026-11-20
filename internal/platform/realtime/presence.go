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
// correctly and only the last one closing makes the user offline. The key
// itself lives twice as long as a member, so that Sweep still finds the
// expired members of a node that died without removing them.
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
		pipe.PExpire(ctx, k, 2*p.ttl)
	})
}

// Remove drops connID and returns the live count after the change, in one MULTI.
func (p *PresenceStore) Remove(ctx context.Context, userID, connID string, now time.Time) (int, error) {
	k := p.key(userID)
	return p.countAfter(ctx, k, now, func(pipe goredis.Pipeliner) {
		pipe.ZRem(ctx, k, connID)
	})
}

// Count returns how many of the user's connections are still live at now.
// It does not prune: removing the last expired member is Sweep's job, since
// whoever removes it is the one who announces the user offline.
func (p *PresenceStore) Count(ctx context.Context, userID string, now time.Time) (int, error) {
	n, err := p.c.ZCount(ctx, p.key(userID), "("+strconv.FormatInt(now.UnixMilli(), 10), "+inf").Result()
	return int(n), err
}

// Sweep prunes expired members of every user and returns the users it took
// from online to offline: those whose last live members it removed itself.
// Exactly one caller (node) gets each user, because only one pruning MULTI can
// remove a given member. Connections closed normally are not reported here:
// Remove already counted them.
func (p *PresenceStore) Sweep(ctx context.Context, now time.Time) ([]string, error) {
	ms := strconv.FormatInt(now.UnixMilli(), 10)
	keyPrefix := p.prefix + "presence:u:"
	var offline []string
	iter := p.c.Scan(ctx, 0, keyPrefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		k := iter.Val()
		var removed, live *goredis.IntCmd
		_, err := p.c.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
			removed = pipe.ZRemRangeByScore(ctx, k, "-inf", ms)
			live = pipe.ZCount(ctx, k, "("+ms, "+inf")
			return nil
		})
		if err != nil {
			return offline, err
		}
		if removed.Val() > 0 && live.Val() == 0 {
			offline = append(offline, k[len(keyPrefix):])
		}
	}
	return offline, iter.Err()
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
