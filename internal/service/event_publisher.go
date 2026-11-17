package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"

	"github.com/jinheng0301/petbridge/internal/model"
)

// EventPublisher is the production Publisher (backend TRD 7.3). Room state
// events get the room's next seq; every event goes out on the bus and comes
// back to each node's Hub through Run. When any bus step fails, the event is
// handed to this node's Hub directly, without a seq, so a single node keeps
// working while Redis is down.
type EventPublisher struct {
	bus        Bus
	hub        *Hub
	presence   *PresenceService
	onFallback func(error)
	log        *slog.Logger
}

// envelope is the bus payload; it only travels between api nodes.
type envelope struct {
	Type   string         `json:"type"`
	Kind   EventKind      `json:"kind"`
	RoomID string         `json:"room_id"`
	To     []string       `json:"to"`
	Data   map[string]any `json:"data"`
	Seq    int64          `json:"seq"`
}

// NewEventPublisher builds the publisher; onFallback (may be nil) hears why
// an event was delivered locally.
func NewEventPublisher(bus Bus, hub *Hub, onFallback func(error)) *EventPublisher {
	return &EventPublisher{bus: bus, hub: hub, onFallback: onFallback}
}

// SetLogger sets where delivery panics are logged (nil keeps them silent).
func (p *EventPublisher) SetLogger(l *slog.Logger) { p.log = l }

// SetPresence completes the wiring; presence publishes through this publisher,
// so the two cannot both be constructor arguments.
func (p *EventPublisher) SetPresence(ps *PresenceService) { p.presence = ps }

// Publish implements Publisher. The event is already committed, so a caller
// that hung up must not make it look like Redis failed.
func (p *EventPublisher) Publish(ctx context.Context, e Event) {
	ctx = context.WithoutCancel(ctx)
	e = p.withPresence(ctx, e)
	var seq int64
	if e.Kind == KindState {
		n, err := p.bus.NextSeq(ctx, e.RoomID)
		if err != nil {
			p.fallback(e, err)
			return
		}
		seq = n
	}
	payload, err := json.Marshal(envelope{Type: e.Type, Kind: e.Kind, RoomID: e.RoomID, To: e.To, Data: e.Data, Seq: seq})
	if err == nil {
		err = p.bus.Publish(ctx, payload)
	}
	if err != nil {
		p.fallback(e, err)
	}
}

// withPresence gives member.joined the new member's current presence, since a
// member who joins while already connected never triggers an online broadcast.
func (p *EventPublisher) withPresence(ctx context.Context, e Event) Event {
	if e.Type != EventMemberJoined || p.presence == nil {
		return e
	}
	m, ok := e.Data["member"].(model.MemberInfo)
	if !ok {
		return e
	}
	data := maps.Clone(e.Data)
	data["member"] = model.MemberPresence{MemberInfo: m, Presence: p.presence.Status(ctx, m.UserID)}
	e.Data = data
	return e
}

func (p *EventPublisher) fallback(e Event, err error) {
	p.hub.Deliver(e, 0)
	if p.onFallback != nil {
		p.onFallback(err)
	}
}

// CurrentSeq is the room's last seq, 0 when unknown or Redis is down.
func (p *EventPublisher) CurrentSeq(ctx context.Context, roomID string) int64 {
	n, err := p.bus.CurrentSeq(ctx, roomID)
	if err != nil {
		return 0
	}
	return n
}

// Run delivers bus events to this node's Hub until ctx ends. After the bus
// reconnects, every local connection gets resync: events may have been
// missed, and a Redis restart resets the seqs.
func (p *EventPublisher) Run(ctx context.Context) {
	p.bus.Run(ctx, func(b []byte) {
		// One bad message must not kill the subscriber goroutine, which runs
		// outside any request recovery and would take the process down.
		defer func() {
			if r := recover(); r != nil && p.log != nil {
				p.log.Error("realtime event delivery panicked", "panic", fmt.Sprint(r))
			}
		}()
		var env envelope
		if err := json.Unmarshal(b, &env); err != nil {
			return
		}
		p.hub.Deliver(Event{Type: env.Type, Kind: env.Kind, RoomID: env.RoomID, To: env.To, Data: env.Data}, env.Seq)
	}, func() {
		p.hub.Broadcast(model.Frame{T: model.FrameResync})
	})
}
