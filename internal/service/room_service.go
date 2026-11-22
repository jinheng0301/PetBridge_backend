package service

import (
	"context"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// RoomService answers room queries; B3b extends it with room management.
type RoomService struct {
	rooms      RoomStore
	users      UserStore
	codes      InviteCodeStore
	regenLimit int
	now        func() time.Time
}

// NewRoomService takes the same daily regen limit as ManageService, so the
// remaining count matches what POST /rooms/{id}/codes enforces.
func NewRoomService(rooms RoomStore, users UserStore, codes InviteCodeStore, regenLimit int,
	now func() time.Time) *RoomService {
	return &RoomService{rooms: rooms, users: users, codes: codes, regenLimit: regenLimit, now: now}
}

// Mine returns the user's active rooms, own room first; never nil. The own
// room carries regen_remaining_today (backend TRD 7.8): the daily limit minus
// today's owner_regen codes in the owner's time zone, counted the way
// RegenCode's daily check counts them. Read without a lock, so it can lag a
// concurrent regen; the regen endpoint stays authoritative.
func (s *RoomService) Mine(ctx context.Context, userID string) ([]model.RoomDetail, error) {
	rooms, err := s.rooms.ListForMember(ctx, userID)
	if err != nil {
		return nil, err
	}
	if rooms == nil {
		rooms = []model.RoomDetail{}
	}
	for i := range rooms {
		if !rooms[i].IsOwner {
			continue
		}
		n, err := s.regenRemaining(ctx, userID)
		if err != nil {
			return nil, err
		}
		rooms[i].RegenRemainingToday = &n
	}
	return rooms, nil
}

func (s *RoomService) regenRemaining(ctx context.Context, ownerID string) (int, error) {
	profile, err := profileOf(s.users.LoadProfile(ctx, ownerID, false))
	if err != nil {
		return 0, err
	}
	from, to := LocalDayBounds(s.now(), profile.TZ)
	used, err := s.codes.CountRegenBetween(ctx, ownerID, from, to)
	if err != nil {
		return 0, err
	}
	return max(0, s.regenLimit-used), nil
}
