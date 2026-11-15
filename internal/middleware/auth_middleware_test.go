package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
	"github.com/jinheng0301/petbridge/internal/service"
)

const testKey = "test-signing-key-test-signing-key"

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)} }

type fakeAccounts map[string]string // user id -> status; absent = unknown user

func (f fakeAccounts) AccountStatus(_ context.Context, id string) (string, error) {
	st, ok := f[id]
	if !ok {
		return "", model.ErrNotFound
	}
	return st, nil
}

// authRouter serves a protected /api/v1/_whoami route behind Auth.
func authRouter(c *clock, accts fakeAccounts) (*gin.Engine, *service.Issuer) {
	iss := service.NewIssuer(testKey, 15*time.Minute, c.now)
	r := gin.New()
	r.Use(RequestID(), AccessLog(logger.New("debug", &bytes.Buffer{})), Recovery(logger.New("debug", &bytes.Buffer{})))
	r.GET("/api/v1/_whoami", Auth(iss, accts), func(c *gin.Context) {
		handler.OK(c, http.StatusOK, gin.H{"user_id": handler.UserID(c)})
	})
	return r, iss
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return m
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	e, _ := decode(t, w)["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func TestAuth_RejectsBadAuthorizationHeaders(t *testing.T) {
	c := newClock()
	r, iss := authRouter(c, fakeAccounts{"u1": "active"})
	valid, _ := iss.Sign("u1")
	expired, _ := iss.Sign("u1")
	other, _ := service.NewIssuer("another-key-another-key-another-key", 15*time.Minute, c.now).Sign("u1")

	cases := map[string]string{
		"missing":   "",
		"bare":      "Bearer",
		"bare+sp":   "Bearer ",
		"basic":     "Basic abc",
		"two parts": "Bearer a b",
		"garbage":   "Bearer not-a-jwt",
		"wrong key": "Bearer " + other,
		"alg none":  "Bearer eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJzdWIiOiJ1MSIsImV4cCI6OTk5OTk5OTk5OX0.",
		"no scheme": valid,
	}
	for name, h := range cases {
		req := httptest.NewRequest("GET", "/api/v1/_whoami", nil)
		if h != "" {
			req.Header.Set("Authorization", h)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 401 || errCode(t, w) != "UNAUTHORIZED" {
			t.Errorf("%s: status %d code %q", name, w.Code, errCode(t, w))
		}
	}

	c.advance(16 * time.Minute)
	req := httptest.NewRequest("GET", "/api/v1/_whoami", nil)
	req.Header.Set("Authorization", "Bearer "+expired)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 || errCode(t, w) != "UNAUTHORIZED" {
		t.Errorf("expired: status %d code %q", w.Code, errCode(t, w))
	}
}

func TestAuth_AcceptsAndSetsUserID(t *testing.T) {
	r, iss := authRouter(newClock(), fakeAccounts{"u-123": "active"})
	tok, _ := iss.Sign("u-123")
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req := httptest.NewRequest("GET", "/api/v1/_whoami", nil)
		req.Header.Set("Authorization", scheme+" "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		data, _ := decode(t, w)["data"].(map[string]any)
		if w.Code != 200 || data["user_id"] != "u-123" {
			t.Errorf("%s: status %d body %s", scheme, w.Code, w.Body.String())
		}
	}
}

func TestAuth_RejectsInactiveAndUnknownAccounts(t *testing.T) {
	r, iss := authRouter(newClock(), fakeAccounts{"banned": "banned", "gone": "deleted"})
	cases := map[string]struct {
		uid  string
		code int
		err  string
	}{
		"banned":  {"banned", 403, "ACCOUNT_DISABLED"},
		"deleted": {"gone", 403, "ACCOUNT_DISABLED"},
		"unknown": {"nobody", 401, "UNAUTHORIZED"},
	}
	for name, tc := range cases {
		tok, _ := iss.Sign(tc.uid)
		req := httptest.NewRequest("GET", "/api/v1/_whoami", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.code || errCode(t, w) != tc.err {
			t.Errorf("%s: status %d code %q", name, w.Code, errCode(t, w))
		}
	}
}
