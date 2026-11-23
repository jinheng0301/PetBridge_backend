package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/middleware"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/server"
	"github.com/jinheng0301/petbridge/internal/service"
	"github.com/jinheng0301/petbridge/pkg/idtoken"
	"github.com/jinheng0301/petbridge/pkg/wordfilter"
)

// The handler tests live in an external test package because they need the
// middleware and server packages, which themselves import handler.

const testKey = "test-signing-key-test-signing-key"

var bg = context.Background()

func init() { gin.SetMode(gin.TestMode) }

func okProbe(context.Context) error { return nil }

func count(t *testing.T, p *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(bg, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// newServices builds the real repository -> service stack on a clean test database.
func newServices(t *testing.T) (*service.AuthService, *service.UserService, *service.Issuer, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.Reset(t)
	tx := repository.NewTxManager(pool)
	users := repository.NewUserRepo(pool)
	iss := service.NewIssuer(testKey, 15*time.Minute, time.Now)
	authSvc := service.NewAuthService(users, repository.NewAuthIdentityRepo(pool), repository.NewRefreshTokenRepo(pool),
		tx, iss, 720*time.Hour)
	userSvc := service.NewUserService(users, repository.NewRoomRepo(pool), repository.NewRoomMemberRepo(pool),
		repository.NewInviteCodeRepo(pool), tx, 7, time.Now).
		WithModeration(testModerator(), repository.NewModFlagRepo(pool))
	return authSvc, userSvc, iss, pool
}

// testModerator blocks "bad" and flags "meh" for review.
func testModerator() *service.WordlistModerator {
	return service.NewWordlistModerator(wordfilter.New([]wordfilter.Entry{
		{Term: "bad", Level: wordfilter.Block},
		{Term: "meh", Level: wordfilter.Review},
	}))
}

// newEngine returns an engine with the production middleware chain and one
// route, /x, served by h.
func newEngine(buf *bytes.Buffer, h gin.HandlerFunc) *gin.Engine {
	l := logger.New("debug", buf)
	r := gin.New()
	r.Use(middleware.RequestID(), middleware.AccessLog(l), middleware.Recovery(l))
	r.Any("/x", h)
	return r
}

// app is the full router as main.go assembles it (auth with the login limit,
// /me, /rooms, /codes) on a clean database.
type app struct {
	r    *gin.Engine
	pool *pgxpool.Pool
	logs *bytes.Buffer
}

// newApp builds the full router. counter backs both the join guard and the
// login limit; nil means a fresh in-process counter. Polishing allows 30 a day.
func newApp(t *testing.T, counter ratelimit.Counter) *app {
	t.Helper()
	return newAppQuota(t, counter, 30)
}

// newAppQuota is newApp with a chosen daily polish quota.
func newAppQuota(t *testing.T, counter ratelimit.Counter, polishDaily int) *app {
	t.Helper()
	authSvc, userSvc, iss, pool := newServices(t)
	if counter == nil {
		counter = ratelimit.NewMemory(time.Now)
	}
	users := repository.NewUserRepo(pool)
	rooms := repository.NewRoomRepo(pool)
	codes := repository.NewInviteCodeRepo(pool)
	joinSvc := service.NewJoinService(users, rooms, repository.NewRoomMemberRepo(pool), codes,
		repository.NewBanRepo(pool), repository.NewRewardRepo(pool), repository.NewTxManager(pool),
		ratelimit.NewJoinGuard(counter), service.NopPublisher{},
		service.JoinConfig{CodeTTLDays: 7, RewardCount: 3, RewardLifetimeCap: 20}, time.Now)
	manageSvc := service.NewManageService(users, rooms, repository.NewRoomMemberRepo(pool), codes,
		repository.NewBanRepo(pool), repository.NewTxManager(pool), service.NopPublisher{},
		service.ManageConfig{CodeTTLDays: 7, RegenDailyLimit: 3, LifetimeCap: 20}, time.Now)
	authn := middleware.Auth(iss, userSvc)
	logs := &bytes.Buffer{}
	quiet := logger.New("error", logs)
	inline := service.NewInlinePool(1, func(context.Context, string) error { return nil }, quiet)
	chatSvc := service.NewChatService(repository.NewTxManager(pool), users, repository.NewRoomMemberRepo(pool),
		repository.NewMessageRepo(pool), testModerator(), nopQueue{}, inline, service.NopPublisher{},
		time.Now, quiet).
		WithModFlags(repository.NewModFlagRepo(pool)).
		WithPolish(service.NewQuotaService(ratelimit.NewPolishGuard(counter, 6, polishDaily), nopMeter{},
			service.QuotaConfig{Enabled: true, DailyBudgetMicros: 5_000_000}, time.Now), nopQueue{})
	r := server.NewRouter(logger.New("debug", logs), handler.NewHealthHandler(okProbe, okProbe),
		handler.NewAuthHandler(authSvc, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{}).
			WithLoginLimit(middleware.RateLimitIP(counter, "login", 10, time.Minute)),
		handler.NewUserHandler(userSvc, authn),
		handler.NewRoomHandler(joinSvc, service.NewRoomService(rooms, users, codes, 3, time.Now), authn).WithManage(manageSvc),
		handler.NewCodeHandler(service.NewCodeService(codes, time.Now), authn),
		handler.NewConfigHandler(authn),
		handler.NewChatHandler(chatSvc, authn,
			middleware.RateLimitUser(counter, "msg", 20, time.Minute),
			middleware.RateLimitUser(counter, "msgday", 1000, 24*time.Hour)))
	return &app{r: r, pool: pool, logs: logs}
}

func (a *app) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := a.pool.Exec(bg, q, args...); err != nil {
		t.Fatal(err)
	}
}

// member is a logged-in user, onboarded unless created with bare.
type member struct {
	token, id, roomID, code string
}

// bare logs in without onboarding.
func (a *app) bare(t *testing.T, email string) member {
	t.Helper()
	tok, id := devLogin(t, a.r, email)
	return member{token: tok, id: id}
}

// onboarded logs in and onboards with ageBand; code is the signup code.
func (a *app) onboarded(t *testing.T, email, ageBand string) member {
	t.Helper()
	m := a.bare(t, email)
	body := strings.Replace(onboardBody, "18_plus", ageBand, 1)
	w := call(a.r, "POST", "/api/v1/me/onboarding", body, m.token)
	if w.Code != 200 {
		t.Fatalf("onboarding %s: %d %s", email, w.Code, w.Body.String())
	}
	d := asMap(t, w)["data"].(map[string]any)
	m.roomID = d["room"].(map[string]any)["id"].(string)
	m.code = d["invite_code"].(map[string]any)["code"].(string)
	return m
}

// extraCode stores another unused owner_regen code (fixed value) for m's room.
func (a *app) extraCode(t *testing.T, m member, code string) string {
	t.Helper()
	a.exec(t, `insert into invite_codes(code, room_id, issued_to, source, expires_at)
		values($1,$2,$3,'owner_regen', now() + interval '1 day')`, code, m.roomID, m.id)
	return code
}

func (a *app) join(m member, code string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"code": code})
	return call(a.r, "POST", "/api/v1/rooms/join", string(b), m.token)
}

// failingCounter is a Counter whose backend is always down.
type failingCounter struct{}

var errRedisDown = errors.New("redis down")

func (failingCounter) Incr(context.Context, string, time.Duration) (ratelimit.Hit, error) {
	return ratelimit.Hit{}, errRedisDown
}
func (failingCounter) SetFlag(context.Context, string, time.Duration) error { return errRedisDown }
func (failingCounter) FlagTTL(context.Context, string) (time.Duration, error) {
	return 0, errRedisDown
}
func (failingCounter) Delete(context.Context, string) error { return errRedisDown }

func do(r http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header[k] = []string{v} // bypass canonicalisation/validation of values
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
