package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// ManageConfig holds the numbers owner-side code issuing depends on.
type ManageConfig struct {
	CodeTTLDays     int
	RegenDailyLimit int
	LifetimeCap     int
}

// ManageService implements owner room management and member leave (design 5.6).
// Every operation is one transaction that locks the room row first, so the
// checks that follow read a stable room. Lock order stays
// users -> invite_codes -> rooms -> room_members: nothing here takes a user
// lock, and dissolve takes the room's unused codes before the room.
type ManageService struct {
	users   UserStore
	rooms   RoomStore
	members RoomMemberStore
	codes   InviteCodeStore
	bans    BanStore
	tx      TxRunner
	pub     Publisher
	cfg     ManageConfig
	now     func() time.Time
	newCode func() (string, error) // tests replace it to force collisions
}

func NewManageService(users UserStore, rooms RoomStore, members RoomMemberStore, codes InviteCodeStore,
	bans BanStore, tx TxRunner, pub Publisher, cfg ManageConfig, now func() time.Time) *ManageService {
	return &ManageService{users: users, rooms: rooms, members: members, codes: codes, bans: bans,
		tx: tx, pub: pub, cfg: cfg, now: now, newCode: NewCode}
}

// publish sends events after commit on a context that outlives the request:
// the commit already happened, so a client that hung up must not lose them.
func (s *ManageService) publish(ctx context.Context, events []Event) {
	post := context.WithoutCancel(ctx)
	for _, e := range events {
		s.pub.Publish(post, e)
	}
}

// ownerCheck decides whether userID may run an owner operation on room. A
// room that does not exist, or one the caller has no seat in, is not found, so
// the answer never reveals whether a room exists; a seated non-owner is told
// they are not the owner.
func (s *ManageService) ownerCheck(ctx context.Context, userID string, room model.RoomState, found bool) error {
	if !found {
		return ErrRoomNotFound
	}
	if room.OwnerID == userID {
		return nil
	}
	member, err := s.members.IsActiveMember(ctx, room.ID, userID)
	if err != nil {
		return err
	}
	if member {
		return ErrNotRoomOwner
	}
	return ErrRoomNotFound
}

// lockRoom locks one room and applies ownerCheck.
func (s *ManageService) lockRoom(ctx context.Context, userID, roomID string) (model.RoomState, error) {
	locked, err := s.rooms.LockRooms(ctx, roomID)
	if err != nil {
		return model.RoomState{}, err
	}
	var room model.RoomState
	var found bool
	for _, r := range locked { // LockRooms lower-cases ids, so look the room up by content, not by key
		room, found = r, true
	}
	if err := s.ownerCheck(ctx, userID, room, found); err != nil {
		return model.RoomState{}, err
	}
	return room, nil
}

func summaryOf(room model.RoomState, memberCount int) model.RoomSummary {
	status := room.Status
	if status == "full" && memberCount < room.Capacity {
		status = "open"
	}
	return model.RoomSummary{ID: room.ID, Status: status, MemberCount: memberCount, Capacity: room.Capacity}
}

func userIDs(members []model.MemberInfo) []string {
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.UserID
	}
	return ids
}

// Remove seats-out targetID as the owner of roomID and bans them from it.
// A target who is no longer seated but once was (left, dissolved out, or a
// leave that committed just before this removal) is still banned, without
// events or seat changes: the owner asked for them to stay out. A target who
// never sat in the room is a success without side effects.
func (s *ManageService) Remove(ctx context.Context, ownerID, roomID, targetID string) (model.RoomSummary, error) {
	// Canonical lower-case form, as ids come back from the database: events
	// built below are routed by user id.
	targetID = strings.ToLower(targetID)
	var res model.RoomSummary
	var events []Event
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		res, events = model.RoomSummary{}, nil
		room, err := s.lockRoom(ctx, ownerID, roomID)
		if err != nil {
			return err
		}
		if targetID == room.OwnerID {
			return ErrCannotRemoveOwner
		}
		left, err := s.members.Leave(ctx, room.ID, targetID, "kicked", s.now())
		if err != nil {
			return err
		}
		if !left {
			ever, err := s.members.EverMember(ctx, room.ID, targetID)
			if err != nil {
				return err
			}
			if ever {
				if err := s.bans.Ban(ctx, room.ID, targetID, ownerID); err != nil {
					return err
				}
			}
			res = summaryOf(room, room.MemberCount)
			return nil
		}
		if err := s.bans.Ban(ctx, room.ID, targetID, ownerID); err != nil {
			return err
		}
		n, err := s.rooms.Vacate(ctx, room.ID)
		if err != nil {
			return err
		}
		remaining, err := s.members.ListMembers(ctx, room.ID)
		if err != nil {
			return err
		}
		res = summaryOf(room, n)
		events = append([]Event{memberLeftEvent(room.ID, targetID, "kicked", userIDs(remaining))},
			roomKickedEvents(room.ID, "kicked", []string{targetID})...)
		return nil
	})
	if err != nil {
		return model.RoomSummary{}, err
	}
	s.publish(ctx, events)
	return res, nil
}

// Leave takes userID out of roomID, a room they joined. The owner cannot
// leave (they dissolve instead). Someone who is no longer seated but once was,
// whether they left, were removed or were dissolved out, gets the same
// success, so a removed member cannot tell a removal from a leave.
func (s *ManageService) Leave(ctx context.Context, userID, roomID string) error {
	var events []Event
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		events = nil
		locked, err := s.rooms.LockRooms(ctx, roomID)
		if err != nil {
			return err
		}
		var room model.RoomState
		found := false
		for _, r := range locked {
			room, found = r, true
		}
		if !found {
			return ErrRoomNotFound
		}
		if room.OwnerID == userID {
			return ErrOwnerCannotLeave
		}
		left, err := s.members.Leave(ctx, room.ID, userID, "left", s.now())
		if err != nil {
			return err
		}
		if !left {
			ever, err := s.members.EverMember(ctx, room.ID, userID)
			if err != nil {
				return err
			}
			if !ever {
				return ErrRoomNotFound
			}
			return nil
		}
		if _, err := s.rooms.Vacate(ctx, room.ID); err != nil {
			return err
		}
		remaining, err := s.members.ListMembers(ctx, room.ID)
		if err != nil {
			return err
		}
		events = []Event{memberLeftEvent(room.ID, userID, "left", userIDs(remaining))}
		return nil
	})
	if err != nil {
		return err
	}
	s.publish(ctx, events)
	return nil
}

// Dissolve moves every member out of the owner's room and voids its unused
// codes, leaving the owner alone in an open room. Nobody is banned. The room
// is peeked first, unlocked, so a caller who is not the owner never locks the
// room's codes; then, as join does, all unused codes are locked in code order
// before the room itself.
func (s *ManageService) Dissolve(ctx context.Context, ownerID, roomID string) (model.RoomSummary, error) {
	peek, err := s.rooms.Peek(ctx, roomID)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return model.RoomSummary{}, err
	}
	if err := s.ownerCheck(ctx, ownerID, peek, err == nil); err != nil {
		return model.RoomSummary{}, err
	}

	var res model.RoomSummary
	var events []Event
	err = s.tx.WithTx(ctx, func(ctx context.Context) error {
		res, events = model.RoomSummary{}, nil
		if err := s.codes.LockUnusedForRoom(ctx, peek.ID); err != nil {
			return err
		}
		room, err := s.lockRoom(ctx, ownerID, peek.ID)
		if err != nil {
			return err
		}
		holders, err := s.codes.VoidUnusedForRoom(ctx, room.ID)
		if err != nil {
			return err
		}
		removed, err := s.members.DissolveMembers(ctx, room.ID, s.now())
		if err != nil {
			return err
		}
		if err := s.rooms.Reset(ctx, room.ID); err != nil {
			return err
		}
		res = model.RoomSummary{ID: room.ID, Status: "open", MemberCount: 1, Capacity: room.Capacity}
		events = roomKickedEvents(room.ID, "dissolved", removed)
		if len(holders) > 0 {
			events = append(events, Event{Type: EventCodeInvalidated, RoomID: room.ID, To: holders,
				Data: map[string]any{"room_id": room.ID, "reason": "dissolved"}})
		}
		return nil
	})
	if err != nil {
		return model.RoomSummary{}, err
	}
	s.publish(ctx, events)
	return res, nil
}

// RegenCode issues one owner_regen code for the owner's room if the limits
// allow (design 5.6, TRD 7.8). The room lock serialises concurrent calls on the
// same room, so the counts read below stay true until this transaction ends.
// The owner's user row is read without a lock: a lock here would run against
// join's users-first order. Checks run in a fixed order and the first failure
// names the reason.
func (s *ManageService) RegenCode(ctx context.Context, ownerID, roomID string) (model.CodeInfo, error) {
	var out model.CodeInfo
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		out = model.CodeInfo{}
		room, err := s.lockRoom(ctx, ownerID, roomID)
		if err != nil {
			return err
		}
		profile, err := profileOf(s.users.LoadProfile(ctx, ownerID, false))
		if err != nil {
			return err
		}
		now := s.now()
		if reason, err := s.regenBlocked(ctx, room, profile, now); err != nil || reason != "" {
			if err != nil {
				return err
			}
			return &CodeIssueLimitError{Reason: reason}
		}
		// Microsecond precision and UTC: what the database hands back.
		issued := now.UTC().Truncate(time.Microsecond)
		expires := issued.Add(time.Duration(s.cfg.CodeTTLDays) * 24 * time.Hour)
		for i := 0; i < maxCodeAttempts; i++ {
			code, err := s.newCode()
			if err != nil {
				return err
			}
			inserted, err := s.codes.InsertRegen(ctx, code, room.ID, ownerID, issued, expires)
			if err != nil {
				return err
			}
			if inserted {
				out = model.CodeInfo{Code: code, ExpiresAt: expires}
				return nil
			}
		}
		return errors.New("could not allocate a unique regen code")
	})
	if err != nil {
		return model.CodeInfo{}, err
	}
	return out, nil
}

// regenBlocked returns the first CODE_ISSUE_LIMIT reason that applies, or "".
func (s *ManageService) regenBlocked(ctx context.Context, room model.RoomState, owner model.Profile, now time.Time) (string, error) {
	if room.Status != "open" {
		return ReasonRoomFull, nil
	}
	unused, err := s.codes.CountUnusedValid(ctx, room.ID, now)
	if err != nil {
		return "", err
	}
	if unused >= room.Capacity-room.MemberCount {
		return ReasonEnoughUnused, nil
	}
	from, to := LocalDayBounds(now, owner.TZ)
	today, err := s.codes.CountRegenBetween(ctx, room.OwnerID, from, to)
	if err != nil {
		return "", err
	}
	if today >= s.cfg.RegenDailyLimit {
		return ReasonDailyLimit, nil
	}
	lifetime, err := s.codes.LifetimeCount(ctx, room.OwnerID)
	if err != nil {
		return "", err
	}
	if lifetime >= s.cfg.LifetimeCap {
		return ReasonLifetimeLimit, nil
	}
	return "", nil
}
