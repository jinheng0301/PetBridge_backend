package model

import "encoding/json"

// WebSocket frame types (backend TRD chapter 9).
const (
	FrameHello          = "hello"
	FrameRoomSub        = "room.sub"
	FrameRoomUnsub      = "room.unsub"
	FrameRoomSnapshot   = "room.snapshot"
	FrameMemberJoined   = "member.joined"
	FrameMemberLeft     = "member.left"
	FrameRoomKicked     = "room.kicked"
	FramePresenceUpdate = "presence.update"
	FramePetTap         = "pet.tap"
	FramePetAction      = "pet.action"
	FrameCodeInvalid    = "code.invalidated"
	FrameResync         = "resync"
	FrameServerRestart  = "server.restart"
	FrameError          = "error"
)

// Presence states. Offline members are drawn as a sleeping pet by the client.
const (
	PresenceOnline  = "online"
	PresenceOffline = "offline"
)

// WebSocket close codes used by the server.
const (
	CloseGoingAway = 1001 // server restart
	ClosePolicy    = 1008 // frame rate exceeded or slow consumer
	CloseTooBig    = 1009 // frame over the read limit
	CloseReplaced  = 4001 // a newer connection of the same user took the slot
	// CloseTokenExpired: the access token used for the upgrade expired;
	// refresh it and reconnect at once.
	CloseTokenExpired = 4401
)

// Frame is a server-to-client WebSocket frame. Seq is set only on room state
// events delivered through Redis.
type Frame struct {
	T   string `json:"t"`
	ID  string `json:"id,omitempty"`
	Seq int64  `json:"seq,omitempty"`
	D   any    `json:"d,omitempty"`
}

// ClientFrame is a client-to-server frame; D is decoded per type.
type ClientFrame struct {
	T  string          `json:"t"`
	ID string          `json:"id"`
	D  json.RawMessage `json:"d"`
}

// MemberPresence is a room member with their current presence.
type MemberPresence struct {
	MemberInfo
	Presence string `json:"presence"`
}

// SnapshotRoom has the shape of one GET /rooms/mine item plus presence per member.
type SnapshotRoom struct {
	RoomSummary
	IsOwner bool             `json:"is_owner"`
	Members []MemberPresence `json:"members"`
}

// RoomSnapshot is the payload of room.snapshot.
type RoomSnapshot struct {
	RoomID string       `json:"room_id"`
	Seq    int64        `json:"seq"`
	Room   SnapshotRoom `json:"room"`
}
