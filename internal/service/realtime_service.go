package service

import (
	"context"
	"errors"
	"strings"

	"github.com/jinheng0301/petbridge/internal/model"
)

// SeqReader reads a room's current event seq (EventPublisher in production).
type SeqReader interface {
	CurrentSeq(ctx context.Context, roomID string) int64
}

// RealtimeService answers the WebSocket's room requests: subscription
// snapshots and pet taps.
type RealtimeService struct {
	rooms    RoomStore
	members  RoomMemberStore
	presence *PresenceService
	seqs     SeqReader
	pub      Publisher
}

func NewRealtimeService(rooms RoomStore, members RoomMemberStore, presence *PresenceService,
	seqs SeqReader, pub Publisher) *RealtimeService {
	return &RealtimeService{rooms: rooms, members: members, presence: presence, seqs: seqs, pub: pub}
}

// Snapshot returns the room as the user sees it, with each member's presence.
// The seq is read before the members, so any change after it is either in
// the snapshot or arrives later with a higher seq. A room the user is not
// seated in, or that does not exist, is ErrRoomNotFound.
func (s *RealtimeService) Snapshot(ctx context.Context, userID, roomID string) (model.RoomSnapshot, error) {
	roomID = strings.ToLower(roomID)
	seq := s.seqs.CurrentSeq(ctx, roomID)
	rooms, err := s.rooms.ListForMember(ctx, userID)
	if err != nil {
		return model.RoomSnapshot{}, err
	}
	for _, r := range rooms {
		if r.ID != roomID {
			continue
		}
		members := make([]model.MemberPresence, 0, len(r.Members))
		for _, m := range r.Members {
			members = append(members, model.MemberPresence{MemberInfo: m, Presence: s.presence.Status(ctx, m.UserID)})
		}
		return model.RoomSnapshot{RoomID: roomID, Seq: seq,
			Room: model.SnapshotRoom{RoomSummary: r.RoomSummary, IsOwner: r.IsOwner, Members: members}}, nil
	}
	return model.RoomSnapshot{}, ErrRoomNotFound
}

// PetTap relays a tap on targetID's pet to every other member of the room.
// Both users must be seated and distinct; otherwise ErrRoomNotFound.
func (s *RealtimeService) PetTap(ctx context.Context, userID, roomID, targetID string) error {
	roomID, targetID = strings.ToLower(roomID), strings.ToLower(targetID)
	if _, err := s.rooms.Peek(ctx, roomID); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return ErrRoomNotFound
		}
		return err
	}
	members, err := s.members.ListMembers(ctx, roomID)
	if err != nil {
		return err
	}
	var to []string
	fromSeated, targetSeated := false, false
	for _, m := range members {
		switch m.UserID {
		case userID:
			fromSeated = true
			continue
		case targetID:
			targetSeated = true
		}
		to = append(to, m.UserID)
	}
	if !fromSeated || !targetSeated || userID == targetID {
		return ErrRoomNotFound
	}
	s.pub.Publish(ctx, Event{Type: EventPetAction, Kind: KindTransient, RoomID: roomID, To: to,
		Data: map[string]any{"room_id": roomID, "from_user_id": userID, "target_user_id": targetID, "action": "tap"}})
	return nil
}
