package service

import (
	"context"

	"github.com/jinheng0301/petbridge/internal/model"
)

// Event types (backend TRD chapter 9).
const (
	EventMemberJoined    = "member.joined"
	EventCodeInvalidated = "code.invalidated"
	EventMemberLeft      = "member.left"
	EventRoomKicked      = "room.kicked"
)

// memberLeftEvent tells the remaining members that userID left, was kicked or
// was dissolved out (reason left|kicked|dissolved).
func memberLeftEvent(roomID, userID, reason string, to []string) Event {
	return Event{Type: EventMemberLeft, RoomID: roomID, To: to,
		Data: map[string]any{"room_id": roomID, "user_id": userID, "reason": reason}}
}

// roomKickedEvents builds one room.kicked per removed user, each addressed to that user only.
func roomKickedEvents(roomID, reason string, users []string) []Event {
	var events []Event
	for _, u := range users {
		events = append(events, Event{Type: EventRoomKicked, RoomID: roomID, To: []string{u},
			Data: map[string]any{"room_id": roomID, "reason": reason}})
	}
	return events
}

// Event is a realtime notification for the users in To.
type Event struct {
	Type   string
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
		events = append(events, Event{Type: EventMemberJoined, RoomID: roomID, To: others,
			Data: map[string]any{"room_id": roomID, "member": me}})
	}
	if len(holders) > 0 {
		events = append(events, Event{Type: EventCodeInvalidated, RoomID: roomID, To: holders,
			Data: map[string]any{"room_id": roomID, "reason": "room_full"}})
	}
	return events
}
