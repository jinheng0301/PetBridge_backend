package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/repository"
)

// Like the other race tests these run on a bare TxManager: a deadlock must
// fail the test instead of being retried away.
func (e *checkinEnv) raceService(t *testing.T, want int) *CheckinService {
	t.Helper()
	pool, _ := e.concurrentPool(t, want)
	return NewCheckinService(repository.NewTxManager(pool), repository.NewUserRepo(pool), repository.NewRoomRepo(pool),
		repository.NewRoomMemberRepo(pool), repository.NewCheckinRepo(pool), e.pub, func() time.Time { return e.at })
}

func TestConcurrentCheckin_FiveMembersOneBonus(t *testing.T) {
	e := newCheckinEnv(t)
	for round := 0; round < 5; round++ {
		owner := e.user(t, fmt.Sprintf("o%d@x.test", round), "18_plus")
		room := e.roomOf(t, owner)
		people := append([]string{owner}, e.seat(t, owner, 4)...)
		svc := e.raceService(t, 5)
		var bonuses atomic.Int32
		errs := raceAll(len(people), func(i int) error {
			res, err := svc.Checkin(bg, people[i], room)
			if res.BonusGained == FullHouseBonus {
				bonuses.Add(1)
			}
			return err
		})
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d person %d: %v", round, i, err)
			}
		}
		if n := e.energy(t, room); n != 70 || bonuses.Load() != 1 {
			t.Fatalf("round %d: energy %d, %d bonuses", round, n, bonuses.Load())
		}
	}
}

func TestConcurrentCheckin_SameUser(t *testing.T) {
	const calls = 20
	e := newCheckinEnv(t)
	u := e.user(t, "a@x.test", "18_plus")
	room := e.roomOf(t, u)
	svc := e.raceService(t, calls)
	var fresh atomic.Int32
	for i, err := range raceAll(calls, func(int) error {
		res, err := svc.Checkin(bg, u, room)
		if err == nil && !res.Already {
			fresh.Add(1)
		}
		return err
	}) {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := e.energy(t, room); n != 10 || fresh.Load() != 1 {
		t.Fatalf("energy %d, %d fresh", n, fresh.Load())
	}
}

// Check-ins race a removal and two joins; nothing may deadlock and the energy
// must equal what was granted.
func TestConcurrentCheckin_WithRemoveAndJoin(t *testing.T) {
	const rounds, joiners = 10, 2
	e := newCheckinEnv(t)
	r := e.manageRace(t, 8, ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 3, LifetimeCap: 20})
	svc := e.raceService(t, 8)
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()
	for round := 0; round < rounds; round++ {
		owner := e.user(t, fmt.Sprintf("r%d@x.test", round), "18_plus")
		room := e.roomOf(t, owner)
		members := e.seat(t, owner, 2)
		users, codes := e.challengers(t, owner, joiners)
		people := append([]string{owner}, members...)
		var granted atomic.Int32
		errs := raceAll(len(people)+1+joiners, func(i int) error {
			switch {
			case i < len(people):
				res, err := svc.Checkin(ctx, people[i], room)
				granted.Add(int32(res.EnergyGained + res.BonusGained))
				return err
			case i == len(people):
				_, err := r.mgr.Remove(ctx, owner, room, members[0])
				return err
			default:
				k := i - len(people) - 1
				_, err := r.join.Join(ctx, users[k], testIP, codes[k])
				return err
			}
		})
		// People are the owner, members[0] (removed meanwhile) and members[1];
		// then the removal and the joins. Only the removed member may lose.
		for i, err := range errs {
			if err != nil && !(i == 1 && errors.Is(err, ErrRoomNotFound)) {
				t.Fatalf("round %d op %d: %v", round, i, err)
			}
		}
		if granted.Load() < 2*CheckinEnergy {
			t.Fatalf("round %d: only %d energy granted", round, granted.Load())
		}
		if n := e.energy(t, room); n != int(granted.Load()) {
			t.Fatalf("round %d: energy %d, granted %d", round, n, granted.Load())
		}
		e.assertConsistent(t, room)
	}
}
