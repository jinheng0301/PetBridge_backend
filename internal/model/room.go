package model

import "time"

// InviteCode is a row of invite_codes.
type InviteCode struct {
	Code      string
	RoomID    string
	IssuedTo  *string
	UsedBy    *string
	Source    string
	Status    string
	ExpiresAt time.Time
}

// RoomState is the repository's view of a room for join checks. Internal only.
type RoomState struct {
	ID          string
	OwnerID     string
	MemberCount int
	Capacity    int
	Status      string
}

type RoomSummary struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	MemberCount int    `json:"member_count"`
	Capacity    int    `json:"capacity"`
}

type MemberInfo struct {
	UserID   string  `json:"user_id"`
	Nickname *string `json:"nickname"`
	PetType  *string `json:"pet_type"`
	SeatNo   int     `json:"seat_no"`
	Role     string  `json:"role"`
}

type RoomDetail struct {
	RoomSummary
	IsOwner bool         `json:"is_owner"`
	Members []MemberInfo `json:"members"`
}

type JoinResult struct {
	Room    RoomSummary  `json:"room"`
	SeatNo  int          `json:"seat_no"`
	Members []MemberInfo `json:"members"`
	Rewards []CodeInfo   `json:"rewards"`
}

type CodeListItem struct {
	Code      string    `json:"code"`
	Source    string    `json:"source"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
	Room      RoomInfo  `json:"room"`
}

// RoomResult is the body of the remove and dissolve responses.
type RoomResult struct {
	Room RoomSummary `json:"room"`
}
