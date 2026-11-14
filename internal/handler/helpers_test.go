package handler_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/middleware"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/service"
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
		repository.NewInviteCodeRepo(pool), tx, 7, time.Now)
	return authSvc, userSvc, iss, pool
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

func do(r http.Handler, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header[k] = []string{v} // bypass canonicalisation/validation of values
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
