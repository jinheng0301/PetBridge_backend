package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jinheng0301/petbridge/internal/model"
)

// CheckinService runs daily check-ins and lists a room's decorations (B6b spec 7, 8).
type CheckinService struct {
	tx      TxRunner
	users   UserStore
	rooms   RoomStore
	members RoomMemberStore
	store   CheckinStore
	pub     Publisher
	now     func() time.Time
}

func NewCheckinService(tx TxRunner, users UserStore, rooms RoomStore, members RoomMemberStore,
	store CheckinStore, pub Publisher, now func() time.Time) *CheckinService {
	return &CheckinService{tx: tx, users: users, rooms: rooms, members: members, store: store, pub: pub, now: now}
}

// Checkin records today's check-in of userID in roomID, grants the energy and
// any full-house bonus, and unlocks what the new total reaches. The room lock
// queues the room's check-ins and keeps its members stable, so of two last
// members checking in together one always sees the other. A repeat on the same
// local day changes nothing and publishes nothing. Not being seated, or an
// unknown room, is ErrRoomNotFound.
func (s *CheckinService) Checkin(ctx context.Context, userID, roomID string) (model.CheckinResult, error) {
	var res model.CheckinResult
	var events []Event
	err := s.tx.WithTx(ctx, func(ctx context.Context) error {
		res, events = model.CheckinResult{}, nil
		locked, err := s.rooms.LockRooms(ctx, roomID)
		if err != nil {
			return err
		}
		room, found := locked[strings.ToLower(roomID)]
		if !found {
			return ErrRoomNotFound
		}
		members, err := s.members.ListMembers(ctx, room.ID)
		if err != nil {
			return err
		}
		seated := userIDs(members)
		if !slices.Contains(seated, userID) {
			return ErrRoomNotFound
		}
		// Read without a lock: the room lock is already held and join takes users first.
		profile, err := profileOf(s.users.LoadProfile(ctx, userID, false))
		if err != nil {
			return err
		}
		// After the lock, so check-in times in one room never go backwards.
		now := s.now().UTC().Truncate(time.Microsecond)
		day, expires := LocalDay(now, profile.TZ)
		res = model.CheckinResult{CheckinExpiresAt: expires, Unlocked: []string{}}

		inserted, err := s.store.InsertCheckin(ctx, room.ID, userID, day, CheckinEnergy, now)
		if err != nil {
			return err
		}
		if !inserted {
			res.Already = true
			if res.EnergyTotal, err = s.store.Energy(ctx, room.ID); err != nil {
				return err
			}
			res.NextThreshold = NextThreshold(res.EnergyTotal)
			return nil
		}
		res.EnergyGained = CheckinEnergy
		full, err := s.store.ClaimFullHouse(ctx, room.ID, now, FullHouseWindow)
		if err != nil {
			return err
		}
		if full {
			res.BonusGained = FullHouseBonus
		}
		if res.EnergyTotal, err = s.store.AddEnergy(ctx, room.ID, res.EnergyGained+res.BonusGained, now); err != nil {
			return err
		}
		res.NextThreshold = NextThreshold(res.EnergyTotal)
		reached := ItemsReached(res.EnergyTotal)
		added, err := s.store.UnlockDecor(ctx, room.ID, reached, now)
		if err != nil {
			return err
		}
		for _, item := range reached { // catalog order
			if slices.Contains(added, item) {
				res.Unlocked = append(res.Unlocked, item)
			}
		}
		events = checkinEvents(room.ID, userID, seated, res, now)
		return nil
	})
	if err != nil {
		return model.CheckinResult{}, err
	}
	// The commit already happened: a client that hung up must not lose the events.
	post := context.WithoutCancel(ctx)
	for _, e := range events {
		s.pub.Publish(post, e)
	}
	return res, nil
}

// Decor lists the whole catalog with what the room has unlocked. Items the
// catalog no longer knows are left out.
func (s *CheckinService) Decor(ctx context.Context, userID, roomID string) (model.DecorList, error) {
	seated, err := s.members.IsActiveMember(ctx, roomID, userID)
	if err != nil {
		return model.DecorList{}, err
	}
	if !seated {
		return model.DecorList{}, ErrRoomNotFound
	}
	total, err := s.store.Energy(ctx, roomID)
	if err != nil {
		return model.DecorList{}, err
	}
	unlocked, err := s.store.ListDecor(ctx, roomID)
	if err != nil {
		return model.DecorList{}, err
	}
	at := map[string]*time.Time{}
	for i := range unlocked {
		at[unlocked[i].ItemID] = &unlocked[i].UnlockedAt
	}
	catalog := DecorCatalog()
	items := make([]model.DecorListItem, len(catalog))
	for i, d := range catalog {
		items[i] = model.DecorListItem{ItemID: d.ID, Threshold: d.Threshold, UnlockedAt: at[d.ID]}
	}
	return model.DecorList{EnergyTotal: total, NextThreshold: NextThreshold(total), Items: items}, nil
}
