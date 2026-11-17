package service

import (
	"context"
	"slices"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// PresenceService decides when a user goes online or offline and tells their
// roommates (backend TRD 7.3). A user is online while any connection, on any
// node, is live; only the first connection announces online and only the last
// one closing announces offline. While the store (Redis) is down it falls back
// to this node's connection count, which is exact on a single node.
//
// Callers register a connection in the Hub before Connected and unregister it
// before Disconnected, so the fallback counts already include the change.
type PresenceService struct {
	store PresenceStore
	hub   *Hub
	rooms RoomStore
	pub   Publisher
	now   func() time.Time
}

func NewPresenceService(store PresenceStore, hub *Hub, rooms RoomStore, pub Publisher,
	_ RealtimeConfig, now func() time.Time) *PresenceService {
	return &PresenceService{store: store, hub: hub, rooms: rooms, pub: pub, now: now}
}

// Connected records a new connection and announces online if it is the user's first.
func (p *PresenceService) Connected(ctx context.Context, userID, connID string) {
	// The count comes back from the same atomic step as the add, so a
	// concurrent close of another connection cannot slip in between.
	n, err := p.store.Add(ctx, userID, connID, p.now())
	if err != nil {
		n = p.hub.LocalCount(userID)
	}
	if n == 1 {
		p.broadcast(ctx, userID, model.PresenceOnline)
	}
}

// Refresh extends a live connection (called on every successful ping).
func (p *PresenceService) Refresh(ctx context.Context, userID, connID string) {
	_, _ = p.store.Add(ctx, userID, connID, p.now())
}

// Disconnected drops a connection and announces offline if none is left.
func (p *PresenceService) Disconnected(ctx context.Context, userID, connID string) {
	n, err := p.store.Remove(ctx, userID, connID, p.now())
	if err != nil {
		n = p.hub.LocalCount(userID)
	}
	if n == 0 {
		p.broadcast(ctx, userID, model.PresenceOffline)
	}
}

// Status is online or offline.
func (p *PresenceService) Status(ctx context.Context, userID string) string {
	n, err := p.store.Count(ctx, userID, p.now())
	if err != nil {
		n = p.hub.LocalCount(userID)
	}
	if n > 0 {
		return model.PresenceOnline
	}
	return model.PresenceOffline
}

// broadcast sends one presence.update per seated room to the other members.
func (p *PresenceService) broadcast(ctx context.Context, userID, status string) {
	rooms, err := p.rooms.ListForMember(ctx, userID)
	if err != nil {
		return
	}
	for _, r := range rooms {
		var to []string
		for _, m := range r.Members {
			if m.UserID != userID {
				to = append(to, m.UserID)
			}
		}
		if len(to) == 0 {
			continue
		}
		p.pub.Publish(ctx, Event{Type: EventPresenceUpdate, Kind: KindTransient, RoomID: r.ID, To: slices.Clip(to),
			Data: map[string]any{"room_id": r.ID, "user_id": userID, "status": status}})
	}
}
