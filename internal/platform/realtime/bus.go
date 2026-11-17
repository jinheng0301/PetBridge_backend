// Package realtime holds the Redis side of WebSocket delivery: room sequence
// numbers, the ws:events Pub/Sub channel and the presence sorted sets.
package realtime

import (
	"context"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const (
	minBackoff = time.Second
	maxBackoff = 10 * time.Second
)

// Bus numbers room events and carries them between api nodes.
type Bus struct {
	c       *goredis.Client
	channel string
	prefix  string
}

// NewBus builds a Bus on channel; keyPrefix is prepended to every key
// (empty in production, unique per test).
func NewBus(c *goredis.Client, channel, keyPrefix string) *Bus {
	return &Bus{c: c, channel: channel, prefix: keyPrefix}
}

func (b *Bus) seqKey(roomID string) string { return b.prefix + "room:" + roomID + ":seq" }

// NextSeq returns the room's next sequence number.
func (b *Bus) NextSeq(ctx context.Context, roomID string) (int64, error) {
	return b.c.Incr(ctx, b.seqKey(roomID)).Result()
}

// CurrentSeq returns the room's last sequence number, 0 when none was issued.
func (b *Bus) CurrentSeq(ctx context.Context, roomID string) (int64, error) {
	n, err := b.c.Get(ctx, b.seqKey(roomID)).Int64()
	if errors.Is(err, goredis.Nil) {
		return 0, nil
	}
	return n, err
}

// Publish sends payload to every node subscribed to the channel.
func (b *Bus) Publish(ctx context.Context, payload []byte) error {
	return b.c.Publish(ctx, b.channel, payload).Err()
}

// Run subscribes to the channel until ctx ends, passing each payload to
// onMessage. The first subscription is silent; every later one (after the
// connection dropped and came back) calls onReconnect once, since events may
// have been missed meanwhile. Failed attempts back off from 1s up to 10s.
func (b *Bus) Run(ctx context.Context, onMessage func([]byte), onReconnect func()) {
	subscribed := false
	backoff := minBackoff
	for ctx.Err() == nil {
		ps := b.c.Subscribe(ctx, b.channel)
		// Receive blocks on the socket and ignores ctx; closing the PubSub
		// is what unblocks it when ctx ends.
		stop := context.AfterFunc(ctx, func() { _ = ps.Close() })
		for {
			msg, err := ps.Receive(ctx)
			if err != nil {
				break
			}
			switch m := msg.(type) {
			case *goredis.Subscription:
				if m.Kind != "subscribe" {
					continue
				}
				if subscribed {
					onReconnect()
				}
				subscribed, backoff = true, minBackoff
			case *goredis.Message:
				onMessage([]byte(m.Payload))
			}
		}
		stop()
		_ = ps.Close()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}
