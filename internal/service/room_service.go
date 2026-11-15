package service

import (
	"context"

	"github.com/jinheng0301/petbridge/internal/model"
)

// RoomService answers room queries; B3b extends it with room management.
type RoomService struct {
	rooms RoomStore
}

func NewRoomService(rooms RoomStore) *RoomService {
	return &RoomService{rooms: rooms}
}

// Mine returns the user's active rooms, own room first; never nil.
func (s *RoomService) Mine(ctx context.Context, userID string) ([]model.RoomDetail, error) {
	rooms, err := s.rooms.ListForMember(ctx, userID)
	if err != nil {
		return nil, err
	}
	if rooms == nil {
		rooms = []model.RoomDetail{}
	}
	return rooms, nil
}
