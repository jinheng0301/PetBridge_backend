package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/repository"
)

// These tests run on a bare TxManager, not RetryTx: a deadlock must fail the
// test instead of being retried away.

// raceAll starts n goroutines behind a start barrier and collects their errors.
func raceAll(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// manageRace is a join service and a manage service sharing one large pool.
type manageRace struct {
	join *JoinService
	mgr  *ManageService
}

func (e *joinEnv) manageRace(t *testing.T, want int, cfg ManageConfig) manageRace {
	t.Helper()
	pool, _ := e.concurrentPool(t, want)
	clock := func() time.Time { return e.now }
	users, rooms := repository.NewUserRepo(pool), repository.NewRoomRepo(pool)
	members, codes := repository.NewRoomMemberRepo(pool), repository.NewInviteCodeRepo(pool)
	bans, tx := repository.NewBanRepo(pool), repository.NewTxManager(pool)
	return manageRace{
		join: NewJoinService(users, rooms, members, codes, bans, repository.NewRewardRepo(pool), tx, e.guard, e.pub,
			defaultJoinConfig(), clock),
		mgr: NewManageService(users, rooms, members, codes, bans, tx, e.pub, cfg, clock),
	}
}

// seat puts n new users straight into the owner's room, bypassing join, and
// returns their ids.
func (e *joinEnv) seat(t *testing.T, ownerID string, n int) []string {
	t.Helper()
	room := e.roomOf(t, ownerID)
	ids := make([]string, n)
	for i := range ids {
		e.seq++
		ids[i] = e.user(t, fmt.Sprintf("seat-%d@x.test", e.seq), "18_plus")
		e.exec(t, `insert into room_members(room_id, user_id, seat_no, role) values($1,$2,$3,'member')`, room, ids[i], i+2)
	}
	e.exec(t, `update rooms set member_count=member_count+$2 where id=$1`, room, n)
	return ids
}

// challengers creates n onboarded users, each holding its own unused code of
// the owner's room.
func (e *joinEnv) challengers(t *testing.T, ownerID string, n int) (users, codes []string) {
	t.Helper()
	for i := 0; i < n; i++ {
		e.seq++
		users = append(users, e.user(t, fmt.Sprintf("chal-%d@x.test", e.seq), "18_plus"))
		codes = append(codes, e.extraCode(t, ownerID))
	}
	return users, codes
}

func mustBeNilOr(t *testing.T, what string, errs []error, allowed ...error) (ok int) {
	t.Helper()
	for i, err := range errs {
		if err == nil {
			ok++
			continue
		}
		if c := classify(err, allowed...); c != nil {
			t.Errorf("%s %d: %v", what, i, c)
		}
	}
	return ok
}

func TestConcurrent_RemoveVsJoinLastSeat(t *testing.T) {
	const rounds, joiners = 10, 6
	e := newJoinEnv(t)
	r := e.manageRace(t, joiners+4, ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 3, LifetimeCap: 20})
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()

	for round := 0; round < rounds; round++ {
		e.seq++
		owner := e.user(t, fmt.Sprintf("rv-owner-%d@x.test", e.seq), "18_plus")
		room := e.roomOf(t, owner)
		members := e.seat(t, owner, 3) // 4 of 5 seats taken
		users, codes := e.challengers(t, owner, joiners)

		errs := raceAll(joiners+1, func(i int) error {
			if i == joiners {
				_, err := r.mgr.Remove(ctx, owner, room, members[0])
				return err
			}
			_, err := r.join.Join(ctx, users[i], testIP, codes[i])
			return err
		})
		if err := classify(errs[joiners]); err != nil {
			t.Fatalf("round %d remove: %v", round, err)
		}
		won := mustBeNilOr(t, "join", errs[:joiners], ErrRoomFull, ErrCodeInvalid)
		if won > 2 {
			t.Fatalf("round %d: %d joiners got in, at most 2 seats were ever free", round, won)
		}
		e.assertConsistent(t, room)
		if got := e.memberCount(t, room); got != 4+won-1 {
			t.Fatalf("round %d: member_count=%d, want %d", round, got, 4+won-1)
		}
	}
}

func TestConcurrent_DissolveVsJoin(t *testing.T) {
	const rounds, joiners = 10, 4
	e := newJoinEnv(t)
	r := e.manageRace(t, joiners+4, ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 3, LifetimeCap: 20})
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()

	for round := 0; round < rounds; round++ {
		e.seq++
		owner := e.user(t, fmt.Sprintf("dj-owner-%d@x.test", e.seq), "18_plus")
		room := e.roomOf(t, owner)
		e.seat(t, owner, 1)
		users, codes := e.challengers(t, owner, joiners)

		errs := raceAll(joiners+1, func(i int) error {
			if i == joiners {
				_, err := r.mgr.Dissolve(ctx, owner, room)
				return err
			}
			_, err := r.join.Join(ctx, users[i], testIP, codes[i])
			return err
		})
		if err := classify(errs[joiners]); err != nil {
			t.Fatalf("round %d dissolve: %v", round, err)
		}
		mustBeNilOr(t, "join", errs[:joiners], ErrRoomFull, ErrCodeInvalid)
		// Dissolve voids every code and removes every member in one step, so
		// whichever way the race went nobody but the owner can remain.
		if got := e.memberCount(t, room); got != 1 {
			t.Fatalf("round %d: member_count=%d after dissolve, want 1", round, got)
		}
		e.assertConsistent(t, room)
		if n := count(t, e.pool, `select count(*) from invite_codes where room_id=$1 and status='unused'`, room); n != 0 {
			t.Fatalf("round %d: %d unused codes survived dissolve", round, n)
		}
	}
}

func TestConcurrent_DissolveVsFillRoom(t *testing.T) {
	const rounds, joiners = 10, 3
	e := newJoinEnv(t)
	r := e.manageRace(t, joiners+4, ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 3, LifetimeCap: 20})
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()

	for round := 0; round < rounds; round++ {
		e.seq++
		owner := e.user(t, fmt.Sprintf("df-owner-%d@x.test", e.seq), "18_plus")
		room := e.roomOf(t, owner)
		e.seat(t, owner, 3) // one seat left: a joiner who wins fills the room and voids the other codes
		users, codes := e.challengers(t, owner, joiners)

		errs := raceAll(joiners+1, func(i int) error {
			if i == joiners {
				_, err := r.mgr.Dissolve(ctx, owner, room)
				return err
			}
			_, err := r.join.Join(ctx, users[i], testIP, codes[i])
			return err
		})
		if err := classify(errs[joiners]); err != nil {
			t.Fatalf("round %d dissolve: %v", round, err)
		}
		mustBeNilOr(t, "join", errs[:joiners], ErrRoomFull, ErrCodeInvalid)
		if got := e.memberCount(t, room); got != 1 {
			t.Fatalf("round %d: member_count=%d, want 1", round, got)
		}
		e.assertConsistent(t, room)
	}
}

func TestConcurrent_RegenSpaceBound(t *testing.T) {
	const callers = 10
	e := newJoinEnv(t)
	r := e.manageRace(t, callers+4, ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 10, LifetimeCap: 20})
	owner, room := e.owner(t) // 4 free seats, 1 unused signup code
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()

	errs := raceAll(callers, func(int) error {
		_, err := r.mgr.RegenCode(ctx, owner, room)
		return err
	})
	ok := 0
	for i, err := range errs {
		var lim *CodeIssueLimitError
		switch {
		case err == nil:
			ok++
		case asLimit(err, &lim) && lim.Reason == ReasonEnoughUnused:
		default:
			t.Errorf("call %d: %v", i, err)
		}
	}
	if ok != 3 {
		t.Fatalf("%d regens succeeded, want exactly 3 (4 free seats - 1 existing code)", ok)
	}
	if n := count(t, e.pool, `select count(*) from invite_codes where room_id=$1 and status='unused'`, room); n != 4 {
		t.Fatalf("unused codes=%d, want 4 = free seats", n)
	}
}

func TestConcurrent_RegenDailyBound(t *testing.T) {
	const callers = 10
	e := newJoinEnv(t)
	r := e.manageRace(t, callers+4, ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 2, LifetimeCap: 20})
	owner, room := e.owner(t)
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()

	errs := raceAll(callers, func(int) error {
		_, err := r.mgr.RegenCode(ctx, owner, room)
		return err
	})
	ok := 0
	for i, err := range errs {
		var lim *CodeIssueLimitError
		switch {
		case err == nil:
			ok++
		case asLimit(err, &lim) && lim.Reason == ReasonDailyLimit:
		default:
			t.Errorf("call %d: %v", i, err)
		}
	}
	if ok != 2 {
		t.Fatalf("%d regens succeeded, want exactly 2 (daily limit)", ok)
	}
}

func TestConcurrent_LeaveVsRemoveSameMember(t *testing.T) {
	const rounds = 10
	e := newJoinEnv(t)
	r := e.manageRace(t, 8, ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 3, LifetimeCap: 20})
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()

	for round := 0; round < rounds; round++ {
		e.seq++
		owner := e.user(t, fmt.Sprintf("lr-owner-%d@x.test", e.seq), "18_plus")
		room := e.roomOf(t, owner)
		m := e.seat(t, owner, 1)[0]

		errs := raceAll(2, func(i int) error {
			if i == 0 {
				return r.mgr.Leave(ctx, m, room)
			}
			_, err := r.mgr.Remove(ctx, owner, room, m)
			return err
		})
		mustBeNilOr(t, "op", errs)
		if got := e.memberCount(t, room); got != 1 {
			t.Fatalf("round %d: member_count=%d, want 1 (decremented exactly once)", round, got)
		}
		e.assertConsistent(t, room)
		if n := count(t, e.pool, `select count(*) from room_members where room_id=$1 and user_id=$2 and left_at is not null`, room, m); n != 1 {
			t.Fatalf("round %d: %d ended rows for the member, want 1", round, n)
		}
		// Whichever commits first, the owner's removal must leave a ban.
		if n := count(t, e.pool, `select count(*) from room_bans where room_id=$1 and user_id=$2`, room, m); n != 1 {
			t.Fatalf("round %d: ban rows=%d, want 1 whether leave or remove won", round, n)
		}
	}
}

// asLimit is errors.As for the regen limit error.
func asLimit(err error, target **CodeIssueLimitError) bool {
	return errors.As(err, target)
}
