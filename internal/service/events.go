package service

import (
	"context"

	"github.com/jinheng0301/petbridge/internal/model"
)

// Event types (backend TRD chapter 9).
const (
	EventMemberJoined      = "member.joined"
	EventCodeInvalidated   = "code.invalidated"
	EventMemberLeft        = "member.left"
	EventRoomKicked        = "room.kicked"
	EventPresenceUpdate    = "presence.update"
	EventPetAction         = "pet.action"
	EventMessageNew        = "message.new"
	EventMessageTranslated = "message.translated"
	EventMessagePolished   = "message.polished"
	EventMessageRemoved    = "message.removed"
)

// EventKind decides how an event is numbered and routed.
type EventKind int

const (
	// KindState changes room state for every seated member; it gets a room seq.
	KindState EventKind = iota
	// KindTransient is room-scoped but carries no state the snapshot misses (presence, pet actions); no seq.
	KindTransient
	// KindPersonal is addressed to individual users whether or not they subscribed the room; no seq.
	KindPersonal
)

// memberLeftEvent tells the remaining members that userID left, was kicked or
// was dissolved out (reason left|kicked|dissolved).
func memberLeftEvent(roomID, userID, reason string, to []string) Event {
	return Event{Type: EventMemberLeft, Kind: KindState, RoomID: roomID, To: to,
		Data: map[string]any{"room_id": roomID, "user_id": userID, "reason": reason}}
}

// roomKickedEvents builds one room.kicked per removed user, each addressed to that user only.
func roomKickedEvents(roomID, reason string, users []string) []Event {
	var events []Event
	for _, u := range users {
		events = append(events, Event{Type: EventRoomKicked, Kind: KindPersonal, RoomID: roomID, To: []string{u},
			Data: map[string]any{"room_id": roomID, "reason": reason}})
	}
	return events
}

// messageRemovedEvent tells one receiver to drop a message card that
// moderation hid from them (spec 5.8).
func messageRemovedEvent(roomID, messageID, to string) Event {
	return Event{Type: EventMessageRemoved, Kind: KindTransient, RoomID: roomID, To: []string{to},
		Data: map[string]any{"room_id": roomID, "message_id": messageID}}
}

// Event is a realtime notification for the users in To.
type Event struct {
	Type   string
	Kind   EventKind
	RoomID string
	To     []string
	Data   map[string]any
}

// NopPublisher drops every event. B4 replaces it with the Redis-backed one.
type NopPublisher struct{}

func (NopPublisher) Publish(context.Context, Event) {}

// joinEvents builds the events of a committed join: member.joined for the
// other active members, code.invalidated for holders of codes voided because
// the room filled up.
func joinEvents(roomID, userID string, members []model.MemberInfo, holders []string) []Event {
	var others []string
	var me any
	for _, m := range members {
		if m.UserID == userID {
			me = m
			continue
		}
		others = append(others, m.UserID)
	}
	var events []Event
	if len(others) > 0 {
		events = append(events, Event{Type: EventMemberJoined, Kind: KindState, RoomID: roomID, To: others,
			Data: map[string]any{"room_id": roomID, "member": me}})
	}
	if len(holders) > 0 {
		events = append(events, Event{Type: EventCodeInvalidated, Kind: KindPersonal, RoomID: roomID, To: holders,
			Data: map[string]any{"room_id": roomID, "reason": "room_full"}})
	}
	return events
}
