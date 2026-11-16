package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
	"github.com/jinheng0301/petbridge/internal/repository"
)

const testIP = "203.0.113.7"

// The production limiter must satisfy the port, and NopPublisher the Publisher.
var (
	_ JoinGuard = (*ratelimit.JoinGuard)(nil)
	_ Publisher = NopPublisher{}
)

// fakeGuard records the calls JoinService makes to the rate limiter.
type fakeGuard struct {
	mu         sync.Mutex
	cooldown   time.Duration
	deny       bool
	retryAfter time.Duration
	allowCalls int
	failures   int
	successes  int
	deadCalls  int // Record* calls made with an already-ended context
}

func (g *fakeGuard) Cooldown(context.Context, string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cooldown
}

func (g *fakeGuard) Allow(context.Context, string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allowCalls++
	return !g.deny, g.retryAfter
}

func (g *fakeGuard) RecordFailure(ctx context.Context, _ string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures++
	if ctx.Err() != nil {
		g.deadCalls++
	}
}

func (g *fakeGuard) RecordSuccess(ctx context.Context, _ string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.successes++
	if ctx.Err() != nil {
		g.deadCalls++
	}
}

func (g *fakeGuard) counts() (failures, successes int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failures, g.successes
}

type recordingPublisher struct {
	mu        sync.Mutex
	events    []Event
	deadCalls int // Publish calls made with an already-ended context
}

func (p *recordingPublisher) Publish(ctx context.Context, e Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	if ctx.Err() != nil {
		p.deadCalls++
	}
}

func (p *recordingPublisher) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events, p.deadCalls = nil, 0
}

func (p *recordingPublisher) all() []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Event(nil), p.events...)
}

func (p *recordingPublisher) ofType(typ string) []Event {
	var out []Event
	for _, e := range p.all() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// commitFailTx runs fn inside a real transaction and then fails as a commit
// error would: the transaction rolls back and WithTx returns an error.
type commitFailTx struct{ inner TxRunner }

var errCommitFailed = errors.New("commit failed")

func (c commitFailTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return c.inner.WithTx(ctx, func(ctx context.Context) error {
		if err := fn(ctx); err != nil {
			return err
		}
		return errCommitFailed
	})
}

// cancelAfterTx cancels the request context as soon as the transaction has
// finished, like a client that disconnects right after the commit.
type cancelAfterTx struct {
	inner  TxRunner
	cancel context.CancelFunc
}

func (c cancelAfterTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	err := c.inner.WithTx(ctx, fn)
	c.cancel()
	return err
}

type joinEnv struct {
	pool  *pgxpool.Pool
	now   time.Time
	svc   *JoinService
	auth  *AuthService
	users *UserService
	guard *fakeGuard
	pub   *recordingPublisher
	txm   *repository.TxManager
	mgr   *ManageService
	seq   int
}

func defaultJoinConfig() JoinConfig {
	return JoinConfig{CodeTTLDays: 7, RewardCount: 3, RewardLifetimeCap: 20}
}

func newJoinEnv(t *testing.T) *joinEnv { return newJoinEnvCfg(t, defaultJoinConfig()) }

func newJoinEnvCfg(t *testing.T, cfg JoinConfig) *joinEnv {
	t.Helper()
	pool := dbtest.Reset(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	userRepo := repository.NewUserRepo(pool)
	rooms := repository.NewRoomRepo(pool)
	members := repository.NewRoomMemberRepo(pool)
	codes := repository.NewInviteCodeRepo(pool)
	txm := repository.NewTxManager(pool)
	e := &joinEnv{pool: pool, now: now, guard: &fakeGuard{}, pub: &recordingPublisher{}}
	e.auth = NewAuthService(userRepo, repository.NewAuthIdentityRepo(pool), repository.NewRefreshTokenRepo(pool),
		txm, NewIssuer(testKey, 15*time.Minute, clock), 720*time.Hour)
	e.users = NewUserService(userRepo, rooms, members, codes, txm, cfg.CodeTTLDays, clock)
	e.svc = NewJoinService(userRepo, rooms, members, codes, repository.NewBanRepo(pool), repository.NewRewardRepo(pool),
		txm, e.guard, e.pub, cfg, clock)
	e.txm = txm
	e.mgr = NewManageService(userRepo, rooms, members, codes, repository.NewBanRepo(pool), txm, e.pub,
		ManageConfig{CodeTTLDays: cfg.CodeTTLDays, RegenDailyLimit: 3, LifetimeCap: cfg.RewardLifetimeCap}, clock)
	return e
}

// user creates an onboarded user (with own room and signup code).
func (e *joinEnv) user(t *testing.T, email, ageBand string) string {
	t.Helper()
	id := e.bareUser(t, email)
	in := valid()
	in.AgeBand = ageBand
	if _, err := e.users.Onboard(bg, id, in); err != nil {
		t.Fatal(err)
	}
	return id
}

// bareUser creates a user that has not onboarded.
func (e *joinEnv) bareUser(t *testing.T, email string) string {
	t.Helper()
	id, err := e.auth.EnsureUser(bg, "dev", email)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *joinEnv) signupCode(t *testing.T, userID string) string {
	t.Helper()
	var code string
	if err := e.pool.QueryRow(bg, `select code from invite_codes where issued_to=$1 and source='signup'`, userID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	return code
}

func (e *joinEnv) roomOf(t *testing.T, ownerID string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(bg, `select id::text from rooms where owner_id=$1`, ownerID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// extraCode stores another unused owner_regen code for the owner's room.
func (e *joinEnv) extraCode(t *testing.T, ownerID string) string {
	t.Helper()
	for {
		code, err := NewCode()
		if err != nil {
			t.Fatal(err)
		}
		tag, err := e.pool.Exec(bg, `insert into invite_codes(code, room_id, issued_to, source, expires_at)
			values($1,$2,$3,'owner_regen',$4) on conflict do nothing`, code, e.roomOf(t, ownerID), ownerID, e.now.Add(24*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if tag.RowsAffected() == 1 {
			return code
		}
	}
}

func (e *joinEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(bg, q, args...); err != nil {
		t.Fatal(err)
	}
}

func (e *joinEnv) join(userID, code string) (model.JoinResult, error) {
	return e.svc.Join(bg, userID, testIP, code)
}

func (e *joinEnv) mustJoin(t *testing.T, userID, code string) model.JoinResult {
	t.Helper()
	res, err := e.join(userID, code)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	return res
}

func (e *joinEnv) codeStatus(t *testing.T, code string) string {
	t.Helper()
	var st string
	if err := e.pool.QueryRow(bg, `select status from invite_codes where code=$1`, code).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (e *joinEnv) memberCount(t *testing.T, roomID string) int {
	t.Helper()
	return count(t, e.pool, `select member_count from rooms where id=$1`, roomID)
}

func (e *joinEnv) rewardCodes(t *testing.T, userID string) int {
	t.Helper()
	return count(t, e.pool, `select count(*) from invite_codes where issued_to=$1 and source='join_reward'`, userID)
}
