package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/repository"
)

// concurrentTimeout bounds every concurrency test as a whole.
const concurrentTimeout = 30 * time.Second

// raceJoins starts n goroutines behind a shared start barrier and waits for
// all of them. fn's results land in their own slots, so no locking is needed;
// nothing in a goroutine calls t.Fatal, so an early failure cannot leak one.
func raceJoins(ctx context.Context, n int, fn func(ctx context.Context, i int) (model.JoinResult, error)) ([]model.JoinResult, []error) {
	results := make([]model.JoinResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = fn(ctx, i)
		}(i)
	}
	close(start)
	wg.Wait()
	return results, errs
}

// classify returns nil for success or one of the allowed business errors and
// a descriptive error for anything else (deadlocks, serialization failures,
// unique violations, timeouts...).
func classify(err error, allowed ...error) error {
	if err == nil {
		return nil
	}
	for _, a := range allowed {
		if errors.Is(err, a) {
			return nil
		}
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return fmt.Errorf("database error SQLSTATE %s: %w", pg.Code, err)
	}
	return fmt.Errorf("unexpected error: %w", err)
}

// concurrentService builds a JoinService on its own pool large enough to run
// up to want transactions at once, capped by the server's free connection
// slots. It returns the service and the pool size actually used.
func (e *joinEnv) concurrentService(t *testing.T, want int) (*JoinService, int) {
	t.Helper()
	maxConns := count(t, e.pool, `select setting::int from pg_settings where name='max_connections'`)
	inUse := count(t, e.pool, `select count(*) from pg_stat_activity`)
	size := min(want, maxConns-inUse-10) // leave room for superuser slots and e.pool growth
	cfg, err := pgxpool.ParseConfig(dbtest.URL())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = int32(max(size, 1))
	cfg.MinConns = cfg.MaxConns
	pool, err := pgxpool.NewWithConfig(bg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Open every connection up front so the race measures locking, not dialing.
	conns := make([]*pgxpool.Conn, 0, cfg.MaxConns)
	for i := int32(0); i < cfg.MaxConns; i++ {
		c, err := pool.Acquire(bg)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
	clock := func() time.Time { return e.now }
	svc := NewJoinService(repository.NewUserRepo(pool), repository.NewRoomRepo(pool), repository.NewRoomMemberRepo(pool),
		repository.NewInviteCodeRepo(pool), repository.NewBanRepo(pool), repository.NewRewardRepo(pool),
		repository.NewTxManager(pool), e.guard, e.pub, defaultJoinConfig(), clock)
	return svc, int(cfg.MaxConns)
}

func (e *joinEnv) activeRooms(t *testing.T, userID string) int {
	t.Helper()
	return count(t, e.pool, `select count(*) from room_members where user_id=$1 and left_at is null`, userID)
}

// assertRoomConsistent checks member_count against the active rows.
func (e *joinEnv) assertRoomConsistent(t *testing.T, roomID string) {
	t.Helper()
	mc := e.memberCount(t, roomID)
	active := count(t, e.pool, `select count(*) from room_members where room_id=$1 and left_at is null`, roomID)
	if mc != active {
		t.Fatalf("room %s: member_count=%d but %d active rows", roomID, mc, active)
	}
}

func TestConcurrent_100JoinersForLastSeat(t *testing.T) {
	const joiners = 100
	e := newJoinEnv(t)
	owner := e.user(t, "owner@local", "18_plus")
	room := e.roomOf(t, owner)
	// Three members seated directly, so the only reward grant for this room is the race winner's.
	for i := 0; i < 3; i++ {
		m := e.user(t, fmt.Sprintf("m%d@local", i), "18_plus")
		e.exec(t, `insert into room_members(room_id, user_id, seat_no, role) values($1,$2,$3,'member')`, room, m, i+2)
	}
	e.exec(t, `update rooms set member_count=4 where id=$1`, room)

	users := make([]string, joiners)
	codes := make([]string, joiners)
	for i := range users {
		users[i] = e.user(t, fmt.Sprintf("j%03d@local", i), "18_plus")
		codes[i] = e.extraCode(t, owner)
	}
	svc, poolSize := e.concurrentService(t, joiners+5)
	if poolSize < 50 {
		t.Fatalf("only %d connections available; need at least 50 truly concurrent joiners", poolSize)
	}
	t.Logf("pool size %d for %d joiners", poolSize, joiners)

	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()
	_, errs := raceJoins(ctx, joiners, func(ctx context.Context, i int) (model.JoinResult, error) {
		return svc.Join(ctx, users[i], testIP, codes[i])
	})

	winners := 0
	var winner string
	for i, err := range errs {
		if err == nil {
			winners++
			winner = users[i]
			continue
		}
		if bad := classify(err, ErrRoomFull, ErrCodeInvalid); bad != nil {
			t.Errorf("joiner %d: %v", i, bad)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
	e.assertRoomConsistent(t, room)
	if mc := e.memberCount(t, room); mc != 5 {
		t.Fatalf("member_count = %d, want 5", mc)
	}
	var status string
	if err := e.pool.QueryRow(bg, `select status from rooms where id=$1`, room).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "full" {
		t.Fatalf("status = %q, want full", status)
	}
	if n := count(t, e.pool, `select count(*) from reward_grants where room_id=$1`, room); n != 1 {
		t.Fatalf("reward_grants rows for room = %d, want 1", n)
	}
	if n := count(t, e.pool, `select count(*) from reward_grants where room_id=$1 and user_id=$2`, room, winner); n != 1 {
		t.Fatalf("winner has %d reward grants, want 1", n)
	}
	if n := count(t, e.pool, `select count(*) from invite_codes where room_id=$1 and status='unused'`, room); n != 0 {
		t.Fatalf("%d unused codes left in the full room", n)
	}
	if n := count(t, e.pool, `select count(*) from invite_codes where code = any($1) and status='used'`, codes); n != 1 {
		t.Fatalf("used race codes = %d, want 1", n)
	}
	if n := count(t, e.pool, `select count(*) from invite_codes where code = any($1) and status='void'`, codes); n != joiners-1 {
		t.Fatalf("void race codes = %d, want %d", n, joiners-1)
	}
	for i, u := range users {
		if u == winner {
			continue
		}
		if n := e.activeRooms(t, u); n != 1 {
			t.Fatalf("loser %d sits in %d rooms, want 1 (own)", i, n)
		}
	}
}

func TestConcurrent_SameUserTwoDifferentRooms(t *testing.T) {
	e := newJoinEnv(t)
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()
	for round := 0; round < 20; round++ {
		a := e.user(t, fmt.Sprintf("a%d@local", round), "18_plus")
		c := e.user(t, fmt.Sprintf("c%d@local", round), "18_plus")
		b := e.user(t, fmt.Sprintf("b%d@local", round), "18_plus")
		rooms := []string{e.roomOf(t, a), e.roomOf(t, c)}
		codes := []string{e.signupCode(t, a), e.signupCode(t, c)}

		_, errs := raceJoins(ctx, 2, func(ctx context.Context, i int) (model.JoinResult, error) {
			return e.svc.Join(ctx, b, testIP, codes[i])
		})
		ok, limited := 0, 0
		for i, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrRoomLimit):
				limited++
			default:
				t.Fatalf("round %d request %d: %v", round, i, classify(err))
			}
		}
		if ok != 1 || limited != 1 {
			t.Fatalf("round %d: successes=%d room_limit=%d errs=%v", round, ok, limited, errs)
		}
		if n := e.activeRooms(t, b); n != 2 {
			t.Fatalf("round %d: B sits in %d rooms, want 2", round, n)
		}
		for _, r := range rooms {
			e.assertRoomConsistent(t, r)
		}
		if n := count(t, e.pool, `select count(*) from reward_grants where user_id=$1`, b); n != 1 {
			t.Fatalf("round %d: B has %d reward grants, want 1", round, n)
		}
	}
}

func TestConcurrent_SameUserSameCodeDuplicateSubmit(t *testing.T) {
	const submits = 10
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	room := e.roomOf(t, a)
	code := e.signupCode(t, a)
	before := e.memberCount(t, room)

	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()
	results, errs := raceJoins(ctx, submits, func(ctx context.Context, _ int) (model.JoinResult, error) {
		return e.svc.Join(ctx, b, testIP, code)
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submit %d: %v", i, classify(err))
		}
	}
	for i := 1; i < submits; i++ {
		if !reflect.DeepEqual(results[0], results[i]) {
			t.Fatalf("submit %d differs:\n%+v\nvs\n%+v", i, results[i], results[0])
		}
	}
	if got := e.memberCount(t, room); got != before+1 {
		t.Fatalf("member_count = %d, want %d", got, before+1)
	}
	e.assertRoomConsistent(t, room)
	if n := e.rewardCodes(t, b); n != 3 || len(results[0].Rewards) != 3 {
		t.Fatalf("reward codes in db=%d in result=%d, want 3", n, len(results[0].Rewards))
	}
	if n := count(t, e.pool, `select count(*) from reward_grants where user_id=$1`, b); n != 1 {
		t.Fatalf("reward_grants for B = %d, want 1", n)
	}
	if n := count(t, e.pool, `select count(*) from invite_edges where invitee_id=$1`, b); n != 1 {
		t.Fatalf("invite_edges for B = %d, want 1", n)
	}
	if st := e.codeStatus(t, code); st != "used" {
		t.Fatalf("code status = %q, want used", st)
	}
}

func TestConcurrent_AdultAndMinorSameRoom(t *testing.T) {
	e := newJoinEnv(t)
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()
	for round := 0; round < 20; round++ {
		owner := e.user(t, fmt.Sprintf("o%d@local", round), "18_plus")
		x := e.user(t, fmt.Sprintf("x%d@local", round), "18_plus")
		m := e.user(t, fmt.Sprintf("m%d@local", round), "13_17")
		room := e.roomOf(t, owner)
		joiners := []string{x, m}
		codes := []string{e.signupCode(t, owner), e.extraCode(t, owner)}

		_, errs := raceJoins(ctx, 2, func(ctx context.Context, i int) (model.JoinResult, error) {
			return e.svc.Join(ctx, joiners[i], testIP, codes[i])
		})
		if errs[0] != nil {
			t.Fatalf("round %d: adult join: %v", round, classify(errs[0]))
		}
		if !errors.Is(errs[1], ErrAgeRestricted) {
			t.Fatalf("round %d: minor join = %v, want ErrAgeRestricted", round, classify(errs[1], ErrAgeRestricted))
		}
		if n := count(t, e.pool, `select count(*) from room_members rm join users u on u.id=rm.user_id
			where rm.room_id=$1 and rm.left_at is null and u.age_band <> '18_plus'`, room); n != 0 {
			t.Fatalf("round %d: %d minors seated in the adult room", round, n)
		}
		e.assertRoomConsistent(t, room)
		if n := e.memberCount(t, room); n != 2 {
			t.Fatalf("round %d: member_count = %d, want 2", round, n)
		}
		if st := e.codeStatus(t, codes[1]); st != "unused" {
			t.Fatalf("round %d: minor's code status = %q, want unused", round, st)
		}
	}
}

func TestConcurrent_CrossJoinNoDeadlock(t *testing.T) {
	e := newJoinEnv(t)
	ctx, cancel := context.WithTimeout(bg, concurrentTimeout)
	defer cancel()
	for round := 0; round < 50; round++ {
		// Fresh users each round: the state is reset without dropping the schema.
		a := e.user(t, fmt.Sprintf("a%d@local", round), "18_plus")
		b := e.user(t, fmt.Sprintf("b%d@local", round), "18_plus")
		joiners := []string{a, b}
		codes := []string{e.signupCode(t, b), e.signupCode(t, a)}
		rooms := []string{e.roomOf(t, a), e.roomOf(t, b)}

		roundCtx, roundCancel := context.WithTimeout(ctx, 5*time.Second)
		began := time.Now()
		_, errs := raceJoins(roundCtx, 2, func(ctx context.Context, i int) (model.JoinResult, error) {
			return e.svc.Join(ctx, joiners[i], testIP, codes[i])
		})
		roundCancel()
		if took := time.Since(began); took > 5*time.Second {
			t.Fatalf("round %d took %v", round, took)
		}
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d joiner %d: %v", round, i, classify(err))
			}
		}
		for _, u := range joiners {
			if n := e.activeRooms(t, u); n != 2 {
				t.Fatalf("round %d: user sits in %d rooms, want 2", round, n)
			}
		}
		for _, r := range rooms {
			e.assertRoomConsistent(t, r)
			if n := e.memberCount(t, r); n != 2 {
				t.Fatalf("round %d: member_count = %d, want 2", round, n)
			}
		}
	}
}
