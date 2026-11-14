package auth

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

	"github.com/jinheng0301/petbridge/internal/health"
	"github.com/jinheng0301/petbridge/internal/httpx"
	"github.com/jinheng0301/petbridge/internal/server"
)

func init() { gin.SetMode(gin.TestMode) }

type fakeVerifier struct {
	subject string
	err     error
}

func (f fakeVerifier) Verify(context.Context, string) (string, error) { return f.subject, f.err }

// whoami is a protected test route.
type whoami struct{ iss *Issuer }

func (w whoami) Routes(v1 *gin.RouterGroup) {
	v1.GET("/_whoami", Middleware(w.iss), func(c *gin.Context) {
		httpx.OK(c, http.StatusOK, gin.H{"user_id": UserID(c)})
	})
}

type env struct {
	r    *gin.Engine
	svc  *Service
	pool *pgxpool.Pool
	c    *clock
	logs *bytes.Buffer
}

func okProbe(context.Context) error { return nil }

func newEnv(t *testing.T, devLogin bool, apple, google IDTokenVerifier) *env {
	t.Helper()
	svc, pool, c := setup(t)
	logs := &bytes.Buffer{}
	h := NewHandler(svc, devLogin, apple, google)
	r := server.NewRouter(httpx.NewLogger("debug", logs), health.New(okProbe, okProbe), h, whoami{svc.issuer})
	return &env{r: r, svc: svc, pool: pool, c: c, logs: logs}
}

func (e *env) do(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
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

func data(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	d, _ := decode(t, w)["data"].(map[string]any)
	if d == nil {
		t.Fatalf("no data in %s", w.Body.String())
	}
	return d
}

const devPath = "/api/v1/auth/login/dev"

func TestDevLogin_RouteAbsentWhenDisabled(t *testing.T) {
	e := newEnv(t, false, NotConfiguredVerifier{}, NotConfiguredVerifier{})
	if w := e.do("POST", devPath, `{"email":"a@local"}`); w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDevLogin_NormalizesEmail(t *testing.T) {
	e := newEnv(t, true, NotConfiguredVerifier{}, NotConfiguredVerifier{})
	w1 := e.do("POST", devPath, `{"email":" Dev-A@Local "}`)
	w2 := e.do("POST", devPath, `{"email":"dev-a@local"}`)
	if w1.Code != 200 || w2.Code != 200 {
		t.Fatalf("codes %d %d: %s", w1.Code, w2.Code, w1.Body.String())
	}
	id1 := data(t, w1)["user"].(map[string]any)["id"]
	id2 := data(t, w2)["user"].(map[string]any)["id"]
	if id1 != id2 {
		t.Fatalf("ids differ: %v %v", id1, id2)
	}
	for _, body := range []string{`{"email":""}`, `{"email":"nope"}`, `{"email":"   "}`, `{}`} {
		w := e.do("POST", devPath, body)
		if w.Code != 400 || errCode(t, w) != "VALIDATION_FAILED" {
			t.Errorf("%s: status %d code %q", body, w.Code, errCode(t, w))
		}
	}
}

func TestLogin_ResponseShape(t *testing.T) {
	e := newEnv(t, true, NotConfiguredVerifier{}, NotConfiguredVerifier{})
	w := e.do("POST", devPath, `{"email":"a@local"}`)
	d := data(t, w)
	user, _ := d["user"].(map[string]any)
	if d["access_token"] == "" || d["refresh_token"] == "" || d["expires_in"] != float64(900) ||
		user["id"] == "" || user["onboarded"] != false {
		t.Fatalf("data = %v", d)
	}
}

func TestAppleGoogle_NotConfigured(t *testing.T) {
	e := newEnv(t, true, NotConfiguredVerifier{}, NotConfiguredVerifier{})
	for _, p := range []string{"apple", "google"} {
		w := e.do("POST", "/api/v1/auth/login/"+p, `{"id_token":"abc"}`)
		if w.Code != 501 || errCode(t, w) != "PROVIDER_NOT_CONFIGURED" {
			t.Errorf("%s: status %d code %q", p, w.Code, errCode(t, w))
		}
		if w := e.do("POST", "/api/v1/auth/login/"+p, `{"id_token":""}`); w.Code != 400 {
			t.Errorf("%s empty token: status %d", p, w.Code)
		}
	}
}

func TestAppleGoogle_UsesVerifierSubject(t *testing.T) {
	e := newEnv(t, false, fakeVerifier{subject: "apple-sub-1"}, fakeVerifier{subject: "google-sub-1"})
	for p, sub := range map[string]string{"apple": "apple-sub-1", "google": "google-sub-1"} {
		w := e.do("POST", "/api/v1/auth/login/"+p, `{"id_token":"whatever"}`)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
		if count(t, e.pool, `select count(*) from auth_identities where provider=$1 and subject=$2`, p, sub) != 1 {
			t.Errorf("%s identity not stored", p)
		}
	}
}

func TestAppleGoogle_VerifierRejectsToken(t *testing.T) {
	e := newEnv(t, false, fakeVerifier{err: errors.New("bad signature")}, NotConfiguredVerifier{})
	w := e.do("POST", "/api/v1/auth/login/apple", `{"id_token":"x"}`)
	if w.Code != 401 || errCode(t, w) != "UNAUTHORIZED" {
		t.Fatalf("status %d code %q", w.Code, errCode(t, w))
	}
}

func login(t *testing.T, e *env, email string) (access, refresh, userID string) {
	t.Helper()
	w := e.do("POST", devPath, `{"email":"`+email+`"}`)
	d := data(t, w)
	return d["access_token"].(string), d["refresh_token"].(string), d["user"].(map[string]any)["id"].(string)
}

func TestRefresh_Endpoint(t *testing.T) {
	e := newEnv(t, true, NotConfiguredVerifier{}, NotConfiguredVerifier{})
	_, refresh, uid := login(t, e, "a@local")

	w := e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"`+refresh+`"}`)
	if w.Code != 200 || data(t, w)["refresh_token"] == refresh {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	w = e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"`+refresh+`"}`)
	if w.Code != 401 || errCode(t, w) != "REFRESH_REUSED" {
		t.Fatalf("replay: %d %q", w.Code, errCode(t, w))
	}
	w = e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"unknown"}`)
	if w.Code != 401 || errCode(t, w) != "REFRESH_INVALID" {
		t.Fatalf("unknown: %d %q", w.Code, errCode(t, w))
	}
	if w := e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":""}`); w.Code != 400 {
		t.Fatalf("empty: %d", w.Code)
	}

	_, refresh2, uid2 := login(t, e, "b@local")
	if _, err := e.pool.Exec(bg, `update users set status='banned' where id=$1`, uid2); err != nil {
		t.Fatal(err)
	}
	w = e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"`+refresh2+`"}`)
	if w.Code != 403 || errCode(t, w) != "ACCOUNT_DISABLED" {
		t.Fatalf("banned: %d %q", w.Code, errCode(t, w))
	}
	_ = uid
}

func TestLogout_Returns204(t *testing.T) {
	e := newEnv(t, true, NotConfiguredVerifier{}, NotConfiguredVerifier{})
	_, refresh, _ := login(t, e, "a@local")
	w := e.do("POST", "/api/v1/auth/logout", `{"refresh_token":"`+refresh+`"}`)
	if w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("logout: %d %q", w.Code, w.Body.String())
	}
	if w := e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"`+refresh+`"}`); w.Code != 401 {
		t.Fatalf("refresh after logout: %d", w.Code)
	}
	if w := e.do("POST", "/api/v1/auth/logout", `{"refresh_token":"never-issued"}`); w.Code != 204 {
		t.Fatalf("unknown logout: %d", w.Code)
	}
}

func TestBody_Oversized_And_NotJSON(t *testing.T) {
	e := newEnv(t, true, NotConfiguredVerifier{}, NotConfiguredVerifier{})
	for name, body := range map[string]string{
		"oversized": `{"email":"` + strings.Repeat("a", 70000) + `@local"}`,
		"not json":  `not json`,
		"wrong typ": `{"email":123}`,
		"empty":     ``,
	} {
		w := e.do("POST", devPath, body)
		if w.Code != 400 || errCode(t, w) != "VALIDATION_FAILED" {
			t.Errorf("%s: status %d code %q", name, w.Code, errCode(t, w))
		}
	}
}

func TestTokensNeverLogged(t *testing.T) {
	e := newEnv(t, true, NotConfiguredVerifier{}, NotConfiguredVerifier{})
	access, refresh, _ := login(t, e, "a@local")
	w := e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"`+refresh+`"}`)
	refresh2 := data(t, w)["refresh_token"].(string)
	for _, secret := range []string{access, refresh, refresh2, testKey} {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatalf("secret found in logs")
		}
	}
}

// --- middleware (no database needed) ---

func middlewareRouter(c *clock) (*gin.Engine, *Issuer) {
	iss := NewIssuer(testKey, 15*time.Minute, c.now)
	r := server.NewRouter(httpx.NewLogger("debug", &bytes.Buffer{}), health.New(okProbe, okProbe), whoami{iss})
	return r, iss
}

func TestMiddleware_RejectsBadAuthorizationHeaders(t *testing.T) {
	c := newClock()
	r, iss := middlewareRouter(c)
	valid, _ := iss.Sign("u1")
	expired, _ := iss.Sign("u1")
	other, _ := NewIssuer("another-key-another-key-another-key", 15*time.Minute, c.now).Sign("u1")

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

func TestMiddleware_AcceptsAndSetsUserID(t *testing.T) {
	r, iss := middlewareRouter(newClock())
	tok, _ := iss.Sign("u-123")
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req := httptest.NewRequest("GET", "/api/v1/_whoami", nil)
		req.Header.Set("Authorization", scheme+" "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 || data(t, w)["user_id"] != "u-123" {
			t.Errorf("%s: status %d body %s", scheme, w.Code, w.Body.String())
		}
	}
}
