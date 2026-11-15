package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
)

// --- unit ---

func TestNormalizeCode(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"abcd-efgh", "ABCDEFGH", true},
		{" abcd efgh ", "ABCDEFGH", true},
		{"\tAB-CD\nEF-GH ", "ABCDEFGH", true},
		{"ABCD2345", "ABCD2345", true},
		{"ABCD0EFG", "", false},  // 0 is not in the alphabet
		{"ABCDIEFG", "", false},  // nor is I
		{"ABCD234", "", false},   // too short
		{"ABCD23456", "", false}, // too long
		{"ＡＢＣＤ２３４５", "", false},  // full-width
		{strings.Repeat("A", 10*1024), "", false},
		{"", "", false},
		{"--------", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeCode(c.in)
		if got != c.want || ok != c.ok {
			name := c.in
			if len(name) > 20 {
				name = name[:20] + "..."
			}
			t.Errorf("NormalizeCode(%q) = %q, %v; want %q, %v", name, got, ok, c.want, c.ok)
		}
	}
}

func TestMaskCode(t *testing.T) {
	for in, want := range map[string]string{"ABCD2345": "AB****", "AB": "AB****", "A": "****", "": "****", "宠物码": "宠物****"} {
		if got := MaskCode(in); got != want {
			t.Errorf("MaskCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- integration ---

func TestJoin_Success(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	aRoom, bRoom := e.roomOf(t, a), e.roomOf(t, b)
	code := e.signupCode(t, a)

	res := e.mustJoin(t, b, code)

	if res.Room.ID != aRoom || res.Room.MemberCount != 2 || res.Room.Capacity != 5 || res.Room.Status != "open" || res.SeatNo != 2 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Members) != 2 || res.Members[0].UserID != a || res.Members[0].Role != "owner" ||
		res.Members[1].UserID != b || res.Members[1].SeatNo != 2 || res.Members[1].Role != "member" {
		t.Fatalf("members = %+v", res.Members)
	}
	if e.memberCount(t, aRoom) != 2 {
		t.Fatal("member_count not 2")
	}
	if n := count(t, e.pool, `select count(*) from room_members where room_id=$1 and user_id=$2 and seat_no=2
		and role='member' and left_at is null`, aRoom, b); n != 1 {
		t.Fatalf("seat rows = %d", n)
	}
	if n := count(t, e.pool, `select count(*) from invite_codes where code=$1 and status='used' and used_by=$2 and used_at=$3`,
		code, b, e.now); n != 1 {
		t.Fatal("code not marked used by B")
	}
	if n := count(t, e.pool, `select count(*) from invite_edges where inviter_id=$1 and invitee_id=$2 and code=$3`, a, b, code); n != 1 {
		t.Fatalf("edges = %d", n)
	}
	if n := count(t, e.pool, `select count(*) from reward_grants where user_id=$1 and room_id=$2 and granted_count=3`, b, aRoom); n != 1 {
		t.Fatalf("reward_grants = %d", n)
	}
	wantExp := e.now.Add(7 * 24 * time.Hour)
	if n := count(t, e.pool, `select count(*) from invite_codes where room_id=$1 and issued_to=$2 and source='join_reward'
		and status='unused' and reward_for=$3 and expires_at=$4`, bRoom, b, code, wantExp); n != 3 {
		t.Fatalf("reward codes in B's room = %d", n)
	}
	if len(res.Rewards) != 3 {
		t.Fatalf("rewards = %+v", res.Rewards)
	}
	for _, r := range res.Rewards {
		if !codeRE.MatchString(r.Code) || !r.ExpiresAt.Equal(wantExp) {
			t.Fatalf("reward = %+v", r)
		}
	}
	if f, s := e.guard.counts(); f != 0 || s != 1 {
		t.Fatalf("failures=%d successes=%d", f, s)
	}
	joined := e.pub.ofType(EventMemberJoined)
	if len(joined) != 1 || joined[0].RoomID != aRoom || !reflect.DeepEqual(joined[0].To, []string{a}) {
		t.Fatalf("member.joined = %+v", joined)
	}
	if len(e.pub.ofType(EventCodeInvalidated)) != 0 {
		t.Fatal("unexpected code.invalidated")
	}
}

func TestJoin_CodeNormalization(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	code := e.signupCode(t, a)
	raw := " " + strings.ToLower(code[:4]) + "-" + strings.ToLower(code[4:]) + " "
	res := e.mustJoin(t, b, raw)
	if res.Room.ID != e.roomOf(t, a) || e.codeStatus(t, code) != "used" {
		t.Fatalf("result = %+v", res)
	}
}

func TestJoin_InvalidCases(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, e *joinEnv, a, b string) (joiner, code string)
	}{
		{"unknown code", func(t *testing.T, e *joinEnv, a, b string) (string, string) { return b, "ZZZZ2345" }},
		{"malformed code", func(t *testing.T, e *joinEnv, a, b string) (string, string) { return b, "ABC" }},
		{"own code", func(t *testing.T, e *joinEnv, a, b string) (string, string) { return a, e.signupCode(t, a) }},
		{"used by someone else", func(t *testing.T, e *joinEnv, a, b string) (string, string) {
			c := e.user(t, "c@local", "18_plus")
			code := e.signupCode(t, a)
			e.mustJoin(t, c, code)
			return b, code
		}},
		{"void", func(t *testing.T, e *joinEnv, a, b string) (string, string) {
			code := e.signupCode(t, a)
			e.exec(t, `update invite_codes set status='void' where code=$1`, code)
			return b, code
		}},
		{"status expired", func(t *testing.T, e *joinEnv, a, b string) (string, string) {
			code := e.signupCode(t, a)
			e.exec(t, `update invite_codes set status='expired' where code=$1`, code)
			return b, code
		}},
		{"expires_at == now", func(t *testing.T, e *joinEnv, a, b string) (string, string) {
			code := e.signupCode(t, a)
			e.exec(t, `update invite_codes set expires_at=$2 where code=$1`, code, e.now)
			return b, code
		}},
		{"banned", func(t *testing.T, e *joinEnv, a, b string) (string, string) {
			e.exec(t, `insert into room_bans(room_id, user_id, banned_by) values($1,$2,$3)`, e.roomOf(t, a), b, a)
			return b, e.signupCode(t, a)
		}},
		{"closed room", func(t *testing.T, e *joinEnv, a, b string) (string, string) {
			e.exec(t, `update rooms set status='closed' where id=$1`, e.roomOf(t, a))
			return b, e.signupCode(t, a)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newJoinEnv(t)
			a := e.user(t, "a@local", "18_plus")
			b := e.user(t, "b@local", "18_plus")
			joiner, code := c.setup(t, e, a, b)
			f0, s0 := e.guard.counts()
			events0 := len(e.pub.all())

			_, err := e.join(joiner, code)
			if !errors.Is(err, ErrCodeInvalid) {
				t.Fatalf("err = %v, want ErrCodeInvalid", err)
			}
			if f, s := e.guard.counts(); f != f0+1 || s != s0 {
				t.Fatalf("failures %d->%d successes %d->%d", f0, f, s0, s)
			}
			if len(e.pub.all()) != events0 {
				t.Fatal("events published on failure")
			}
			if n := count(t, e.pool, `select count(*) from room_members where user_id=$1 and left_at is null`, joiner); n != 1 {
				t.Fatalf("joiner sits in %d rooms", n)
			}
		})
	}
}

func TestJoin_NotOnboarded(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	n := e.bareUser(t, "new@local")
	for _, code := range []string{e.signupCode(t, a), "bad"} {
		if _, err := e.join(n, code); !errors.Is(err, ErrOnboardingRequired) {
			t.Fatalf("code %s: err = %v, want ErrOnboardingRequired", MaskCode(code), err)
		}
	}
	if f, s := e.guard.counts(); f != 0 || s != 0 {
		t.Fatalf("failures=%d successes=%d", f, s)
	}
}

func TestJoin_DisabledAccount(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	e.exec(t, `update users set status='banned' where id=$1`, b)
	if _, err := e.join(b, e.signupCode(t, a)); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("err = %v, want ErrAccountDisabled", err)
	}
	if _, err := e.join("00000000-0000-0000-0000-000000000000", e.signupCode(t, a)); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
	if f, _ := e.guard.counts(); f != 0 {
		t.Fatalf("failures=%d", f)
	}
}

func TestJoin_AlreadyMember(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	aRoom := e.roomOf(t, a)
	e.mustJoin(t, b, e.signupCode(t, a))
	second := e.extraCode(t, a)
	rewards0 := e.rewardCodes(t, b)

	_, err := e.join(b, second)
	var am *AlreadyMemberError
	if !errors.As(err, &am) || am.RoomID != aRoom {
		t.Fatalf("err = %v, want AlreadyMemberError{%s}", err, aRoom)
	}
	if e.codeStatus(t, second) != "unused" || e.rewardCodes(t, b) != rewards0 || e.memberCount(t, aRoom) != 2 {
		t.Fatal("already-member attempt changed state")
	}
	if f, _ := e.guard.counts(); f != 0 {
		t.Fatalf("failures=%d", f)
	}
}

func TestJoin_RoomLimit(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	c := e.user(t, "c@local", "18_plus")
	e.mustJoin(t, b, e.signupCode(t, a)) // B: own room + A's room
	code := e.signupCode(t, c)

	if _, err := e.join(b, code); !errors.Is(err, ErrRoomLimit) {
		t.Fatalf("err = %v, want ErrRoomLimit", err)
	}
	if e.codeStatus(t, code) != "unused" || e.memberCount(t, e.roomOf(t, c)) != 1 {
		t.Fatal("room-limit attempt changed state")
	}
	if f, _ := e.guard.counts(); f != 0 {
		t.Fatalf("failures=%d", f)
	}
}

func TestJoin_AgeRestricted(t *testing.T) {
	e := newJoinEnv(t)
	adult := e.user(t, "adult@local", "18_plus")
	minor := e.user(t, "minor@local", "13_17")
	adult2 := e.user(t, "adult2@local", "18_plus")
	minor2 := e.user(t, "minor2@local", "13_17")

	if _, err := e.join(adult2, e.signupCode(t, minor)); !errors.Is(err, ErrAgeRestricted) {
		t.Fatalf("adult into minor room: err = %v", err)
	}
	if _, err := e.join(minor2, e.signupCode(t, adult)); !errors.Is(err, ErrAgeRestricted) {
		t.Fatalf("minor into adult room: err = %v", err)
	}
	if f, _ := e.guard.counts(); f != 0 {
		t.Fatalf("failures=%d", f)
	}
	// Same band still works.
	e.mustJoin(t, minor2, e.signupCode(t, minor))
}

func TestJoin_ErrorPriority(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	e.mustJoin(t, b, e.signupCode(t, a)) // B now sits in 2 rooms, one of them A's
	_, err := e.join(b, e.extraCode(t, a))
	var am *AlreadyMemberError
	if !errors.As(err, &am) {
		t.Fatalf("err = %v, want AlreadyMemberError before ErrRoomLimit", err)
	}
}

func TestJoin_RoomFull_VoidsRemainingCodes(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	aRoom := e.roomOf(t, a)
	codes := []string{e.signupCode(t, a), e.extraCode(t, a), e.extraCode(t, a), e.extraCode(t, a)}
	leftover := e.extraCode(t, a)
	var joiners []string
	for i, code := range codes {
		u := e.user(t, "u"+string(rune('1'+i))+"@local", "18_plus")
		joiners = append(joiners, u)
		res := e.mustJoin(t, u, code)
		if res.SeatNo != i+2 {
			t.Fatalf("joiner %d seat %d", i, res.SeatNo)
		}
		if i < 3 && res.Room.Status != "open" {
			t.Fatalf("joiner %d status %s", i, res.Room.Status)
		}
		if i == 3 && (res.Room.Status != "full" || res.Room.MemberCount != 5) {
			t.Fatalf("last joiner room = %+v", res.Room)
		}
	}
	var status string
	if err := e.pool.QueryRow(bg, `select status from rooms where id=$1`, aRoom).Scan(&status); err != nil || status != "full" {
		t.Fatalf("room status %q err %v", status, err)
	}
	if e.codeStatus(t, leftover) != "void" {
		t.Fatal("leftover code not voided")
	}
	inv := e.pub.ofType(EventCodeInvalidated)
	if len(inv) != 1 || inv[0].RoomID != aRoom || !reflect.DeepEqual(inv[0].To, []string{a}) {
		t.Fatalf("code.invalidated = %+v", inv)
	}
	joined := e.pub.ofType(EventMemberJoined)
	if len(joined) != 4 || !reflect.DeepEqual(joined[3].To, []string{a, joiners[0], joiners[1], joiners[2]}) {
		t.Fatalf("member.joined = %+v", joined)
	}

	u6 := e.user(t, "u6@local", "18_plus")
	if _, err := e.join(u6, leftover); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("voided code: err = %v", err)
	}
	if f, _ := e.guard.counts(); f != 1 {
		t.Fatalf("failures=%d, want 1", f)
	}

	// A room that is at capacity while its code is still unused: ROOM_FULL, not a failure.
	d := e.user(t, "d@local", "18_plus")
	dCode := e.signupCode(t, d)
	e.exec(t, `update rooms set member_count=capacity where id=$1`, e.roomOf(t, d))
	if _, err := e.join(u6, dCode); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("err = %v, want ErrRoomFull", err)
	}
	if f, _ := e.guard.counts(); f != 1 {
		t.Fatalf("ROOM_FULL counted as failure: %d", f)
	}
	if e.codeStatus(t, dCode) != "unused" {
		t.Fatal("ROOM_FULL consumed the code")
	}
}

// Two joiners with different codes race for the last seat. The winner voids
// the loser's code; the lock order must make the loser wait instead of deadlock.
// A blocker transaction holds the room row until both joiners are waiting on
// a lock, so the interleaving that used to deadlock happens every run.
func TestJoin_ConcurrentDifferentCodesForLastSeat(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	aRoom := e.roomOf(t, a)
	for i, code := range []string{e.signupCode(t, a), e.extraCode(t, a), e.extraCode(t, a)} {
		e.mustJoin(t, e.user(t, "f"+string(rune('1'+i))+"@local", "18_plus"), code)
	}
	x, y := e.extraCode(t, a), e.extraCode(t, a)
	j1, j2 := e.user(t, "j1@local", "18_plus"), e.user(t, "j2@local", "18_plus")

	blocker, err := e.pool.Begin(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(bg) //nolint:errcheck
	if _, err := blocker.Exec(bg, `select 1 from rooms where id=$1 for update`, aRoom); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(bg, 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, pair := range [][2]string{{j1, x}, {j2, y}} {
		wg.Add(1)
		go func(i int, uid, code string) {
			defer wg.Done()
			_, errs[i] = e.svc.Join(ctx, uid, testIP, code)
		}(i, pair[0], pair[1])
	}
	deadline := time.Now().Add(10 * time.Second)
	for count(t, e.pool, `select count(*) from pg_stat_activity
		where datname=current_database() and wait_event_type='Lock'`) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("joiners never blocked on locks")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := blocker.Commit(bg); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrRoomFull), errors.Is(err, ErrCodeInvalid):
		default:
			t.Fatalf("unexpected error (deadlock?): %v", err)
		}
	}
	if ok != 1 || e.memberCount(t, aRoom) != 5 {
		t.Fatalf("successes=%d member_count=%d errs=%v", ok, e.memberCount(t, aRoom), errs)
	}
}

func TestJoin_ReplayReturnsSameResult(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	aRoom := e.roomOf(t, a)
	code := e.signupCode(t, a)
	first := e.mustJoin(t, b, code)
	second := e.mustJoin(t, b, code)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("first %+v\nsecond %+v", first, second)
	}
	if len(second.Rewards) != 3 {
		t.Fatalf("replay rewards = %+v", second.Rewards)
	}
	if e.memberCount(t, aRoom) != 2 || e.rewardCodes(t, b) != 3 {
		t.Fatal("replay changed state")
	}
	// Only the real join resets the failure counter; the replay neither counts
	// as a failure nor resets it.
	if f, s := e.guard.counts(); f != 0 || s != 1 {
		t.Fatalf("failures=%d successes=%d, want 0 and 1", f, s)
	}
	if n := len(e.pub.ofType(EventMemberJoined)); n != 1 {
		t.Fatalf("member.joined published %d times", n)
	}
}

// Replaying one's own used code must not reset the failure counter, or a
// member could interleave replays with guesses and never reach the cooldown.
func TestJoin_ReplayDoesNotResetFailureCount(t *testing.T) {
	e := newJoinEnv(t)
	clk := func() time.Time { return e.now }
	e.svc.guard = ratelimit.NewJoinGuard(ratelimit.NewMemory(clk))
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	code := e.signupCode(t, a)
	e.mustJoin(t, b, code)

	for i := 0; i < 4; i++ {
		if _, err := e.join(b, "ZZZZ2345"); !errors.Is(err, ErrCodeInvalid) {
			t.Fatalf("guess %d: err = %v", i, err)
		}
	}
	e.mustJoin(t, b, code) // replay
	if _, err := e.join(b, "ZZZZ2345"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("fifth guess: err = %v", err)
	}
	_, err := e.join(b, code)
	var cd *CooldownError
	if !errors.As(err, &cd) || cd.RetryAfter <= 0 {
		t.Fatalf("err = %v, want CooldownError after 5 failures despite the replay", err)
	}
}

// A client that disconnects right after the commit must not lose the
// limiter bookkeeping or the events: they run on a context that is not cancelled.
func TestJoin_PostCommitWorkSurvivesClientDisconnect(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	inner := e.svc.tx

	ctx, cancel := context.WithCancel(bg)
	e.svc.tx = cancelAfterTx{inner: inner, cancel: cancel}
	if _, err := e.svc.Join(ctx, b, testIP, e.signupCode(t, a)); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(bg)
	e.svc.tx = cancelAfterTx{inner: inner, cancel: cancel2}
	if _, err := e.svc.Join(ctx2, b, testIP, "ZZZZ2345"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("err = %v", err)
	}
	if f, s := e.guard.counts(); f != 1 || s != 1 {
		t.Fatalf("failures=%d successes=%d", f, s)
	}
	if e.guard.deadCalls != 0 || e.pub.deadCalls != 0 || len(e.pub.all()) != 1 {
		t.Fatalf("guard dead calls=%d publisher dead calls=%d events=%d",
			e.guard.deadCalls, e.pub.deadCalls, len(e.pub.all()))
	}
}

func TestJoin_ReplayDeniedAfterLeaving(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	aRoom := e.roomOf(t, a)
	code := e.signupCode(t, a)
	e.mustJoin(t, b, code)
	e.exec(t, `update room_members set left_at=now(), left_reason='left' where room_id=$1 and user_id=$2`, aRoom, b)
	e.exec(t, `update rooms set member_count=member_count-1 where id=$1`, aRoom)

	if _, err := e.join(b, code); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("err = %v, want ErrCodeInvalid", err)
	}
	if f, _ := e.guard.counts(); f != 1 {
		t.Fatalf("failures=%d", f)
	}
}

func TestJoin_RejoinAfterLeavingGivesNoSecondReward(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	aRoom := e.roomOf(t, a)
	e.mustJoin(t, b, e.signupCode(t, a))
	e.exec(t, `update room_members set left_at=now(), left_reason='left' where room_id=$1 and user_id=$2`, aRoom, b)
	e.exec(t, `update rooms set member_count=member_count-1 where id=$1`, aRoom)
	// Void B's reward codes so B's room has space again: only the grant record
	// can stop a second reward now.
	e.exec(t, `update invite_codes set status='void' where issued_to=$1 and source='join_reward'`, b)

	res := e.mustJoin(t, b, e.extraCode(t, a))
	if res.Rewards == nil || len(res.Rewards) != 0 {
		t.Fatalf("rewards = %#v", res.Rewards)
	}
	if e.rewardCodes(t, b) != 3 {
		t.Fatalf("reward codes = %d, want 3", e.rewardCodes(t, b))
	}
}

func TestJoin_RewardsEmptyNotNil(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	aRoom := e.roomOf(t, a)
	e.exec(t, `update rooms set member_count=capacity where id=$1`, e.roomOf(t, b))

	res := e.mustJoin(t, b, e.signupCode(t, a))
	if res.Rewards == nil || len(res.Rewards) != 0 {
		t.Fatalf("rewards = %#v", res.Rewards)
	}
	raw, err := json.Marshal(res)
	if err != nil || !strings.Contains(string(raw), `"rewards":[]`) {
		t.Fatalf("json = %s err = %v", raw, err)
	}
	if n := count(t, e.pool, `select count(*) from reward_grants where user_id=$1 and room_id=$2 and granted_count=0`, b, aRoom); n != 1 {
		t.Fatalf("zero grant rows = %d", n)
	}
}

func TestJoin_RewardsLimitedByLifetimeCap(t *testing.T) {
	cfg := defaultJoinConfig()
	cfg.RewardLifetimeCap = 2
	e := newJoinEnvCfg(t, cfg)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus") // already holds one signup code
	res := e.mustJoin(t, b, e.signupCode(t, a))
	if len(res.Rewards) != 1 || e.rewardCodes(t, b) != 1 {
		t.Fatalf("rewards = %+v", res.Rewards)
	}
}

func TestJoin_RewardCodeCollisionRetries(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	taken := e.signupCode(t, a)
	seq := []string{taken, taken, "RRRRRRR2", "RRRRRRR3", "RRRRRRR4"}
	e.svc.newCode = func() (string, error) { c := seq[0]; seq = seq[1:]; return c, nil }
	res := e.mustJoin(t, b, taken)
	got := []string{res.Rewards[0].Code, res.Rewards[1].Code, res.Rewards[2].Code}
	if !reflect.DeepEqual(got, []string{"RRRRRRR2", "RRRRRRR3", "RRRRRRR4"}) {
		t.Fatalf("rewards = %v", got)
	}

	// Five collisions in a row give up and roll everything back.
	c := e.user(t, "c@local", "18_plus")
	e.svc.newCode = func() (string, error) { return taken, nil }
	code := e.extraCode(t, a)
	if _, err := e.join(c, code); err == nil || errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("err = %v, want allocation error", err)
	}
	if e.codeStatus(t, code) != "unused" || e.memberCount(t, e.roomOf(t, a)) != 2 {
		t.Fatal("failed join was not rolled back")
	}
}

func TestJoin_CooldownAndRateLimitShortCircuit(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	aRoom := e.roomOf(t, a)
	code := e.signupCode(t, a)

	e.guard.cooldown = 90 * time.Second
	_, err := e.join(b, code)
	var cd *CooldownError
	if !errors.As(err, &cd) || cd.RetryAfter != 90*time.Second {
		t.Fatalf("err = %v, want CooldownError{90s}", err)
	}
	if e.guard.allowCalls != 0 {
		t.Fatal("Allow called during cooldown")
	}

	e.guard.cooldown = 0
	e.guard.deny, e.guard.retryAfter = true, time.Minute
	_, err = e.join(b, code)
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != time.Minute {
		t.Fatalf("err = %v, want RateLimitedError{1m}", err)
	}
	if e.codeStatus(t, code) != "unused" || e.memberCount(t, aRoom) != 1 || e.rewardCodes(t, b) != 0 {
		t.Fatal("short-circuited join touched the database")
	}
	if f, s := e.guard.counts(); f != 0 || s != 0 {
		t.Fatalf("failures=%d successes=%d", f, s)
	}
}

func TestJoin_PublishOnlyAfterCommit(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	aRoom := e.roomOf(t, a)
	code := e.signupCode(t, a)
	e.exec(t, `update rooms set member_count=capacity where id=$1`, aRoom)
	if _, err := e.join(b, code); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("err = %v, want ErrRoomFull", err)
	}
	if n := len(e.pub.all()); n != 0 {
		t.Fatalf("published %d events after rollback", n)
	}

	// The whole join succeeds inside the transaction but the commit fails.
	e.exec(t, `update rooms set member_count=4 where id=$1`, aRoom)
	e.svc.tx = commitFailTx{inner: e.svc.tx}
	if _, err := e.join(b, code); !errors.Is(err, errCommitFailed) {
		t.Fatalf("err = %v, want commit failure", err)
	}
	if n := len(e.pub.all()); n != 0 {
		t.Fatalf("published %d events although commit failed", n)
	}
	if _, s := e.guard.counts(); s != 0 {
		t.Fatal("RecordSuccess called although commit failed")
	}
	if e.codeStatus(t, code) != "unused" {
		t.Fatal("failed commit left the code used")
	}
}

func TestJoin_ErrorsNeverContainFullCode(t *testing.T) {
	e := newJoinEnv(t)
	a := e.user(t, "a@local", "18_plus")
	b := e.user(t, "b@local", "18_plus")
	minor := e.user(t, "m@local", "13_17")
	c := e.user(t, "c@local", "18_plus")
	full := e.user(t, "full@local", "18_plus")
	e.exec(t, `update rooms set member_count=capacity where id=$1`, e.roomOf(t, full))

	type attempt struct{ user, code string }
	voided := e.extraCode(t, a)
	e.exec(t, `update invite_codes set status='void' where code=$1`, voided)
	e.mustJoin(t, b, e.signupCode(t, a))
	attempts := []attempt{
		{b, "ZZZZ2345"},            // unknown
		{b, "abcd-efg0"},           // malformed
		{a, e.extraCode(t, a)},     // own code
		{b, voided},                // void
		{b, e.extraCode(t, a)},     // already member
		{b, e.signupCode(t, c)},    // room limit
		{minor, e.extraCode(t, c)}, // age restricted
		{c, e.signupCode(t, full)}, // room full
	}
	for _, at := range attempts {
		_, err := e.join(at.user, at.code)
		if err == nil {
			t.Fatalf("code %s: expected an error", MaskCode(at.code))
		}
		norm, _ := NormalizeCode(at.code)
		msg := strings.ToUpper(err.Error())
		if strings.Contains(msg, strings.ToUpper(at.code)) || (norm != "" && strings.Contains(msg, norm)) {
			t.Fatalf("error %q leaks the code", err.Error())
		}
	}
}
