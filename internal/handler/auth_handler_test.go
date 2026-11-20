package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
	"github.com/jinheng0301/petbridge/internal/server"
	"github.com/jinheng0301/petbridge/internal/service"
	"github.com/jinheng0301/petbridge/pkg/idtoken"
)

type fakeVerifier struct {
	subject string
	err     error
}

func (f fakeVerifier) Verify(context.Context, string) (string, error) { return f.subject, f.err }

type env struct {
	r    *gin.Engine
	svc  *service.AuthService
	pool *pgxpool.Pool
	logs *bytes.Buffer
}

func newEnv(t *testing.T, devLogin bool, apple, google idtoken.Verifier) *env {
	t.Helper()
	svc, _, _, pool := newServices(t)
	logs := &bytes.Buffer{}
	h := handler.NewAuthHandler(svc, devLogin, apple, google)
	r := server.NewRouter(logger.New("debug", logs), handler.NewHealthHandler(okProbe, okProbe), h)
	return &env{r: r, svc: svc, pool: pool, logs: logs}
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
	e := newEnv(t, false, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
	if w := e.do("POST", devPath, `{"email":"a@local"}`); w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestDevLogin_NormalizesEmail(t *testing.T) {
	e := newEnv(t, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
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
	e := newEnv(t, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
	w := e.do("POST", devPath, `{"email":"a@local"}`)
	d := data(t, w)
	user, _ := d["user"].(map[string]any)
	if d["access_token"] == "" || d["refresh_token"] == "" || d["expires_in"] != float64(900) ||
		user["id"] == "" || user["onboarded"] != false {
		t.Fatalf("data = %v", d)
	}
}

func TestAppleGoogle_NotConfigured(t *testing.T) {
	e := newEnv(t, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
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
	e := newEnv(t, false, fakeVerifier{err: errors.New("bad signature")}, idtoken.NotConfiguredVerifier{})
	w := e.do("POST", "/api/v1/auth/login/apple", `{"id_token":"x"}`)
	if w.Code != 401 || errCode(t, w) != "UNAUTHORIZED" {
		t.Fatalf("status %d code %q", w.Code, errCode(t, w))
	}
}

func TestAppleGoogle_ProviderKeysUnavailable(t *testing.T) {
	down := fakeVerifier{err: fmt.Errorf("%w: status 503", idtoken.ErrUnavailable)}
	e := newEnv(t, false, idtoken.NotConfiguredVerifier{}, down)
	w := e.do("POST", "/api/v1/auth/login/google", `{"id_token":"x"}`)
	if w.Code != 503 || errCode(t, w) != "NOT_READY" {
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
	e := newEnv(t, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
	_, refresh, uid := login(t, e, "a@local")

	w := e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"`+refresh+`"}`)
	if w.Code != 200 || data(t, w)["refresh_token"] == refresh {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	lost := data(t, w)["refresh_token"].(string)
	// Within the reuse grace the old token is a retry and gets a new pair...
	w = e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"`+refresh+`"}`)
	if w.Code != 200 {
		t.Fatalf("grace retry: %d %s", w.Code, w.Body.String())
	}
	// ...which revokes the pair the client never stored; presenting that is theft.
	w = e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"`+lost+`"}`)
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
	e := newEnv(t, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
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
	e := newEnv(t, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
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

func TestLogin_RateLimited(t *testing.T) {
	a := newApp(t, nil)
	var refresh string
	for i := 0; i < 10; i++ {
		w := call(a.r, "POST", devPath, `{"email":"a@local"}`, "")
		if w.Code != 200 {
			t.Fatalf("login %d: %d %s", i, w.Code, w.Body.String())
		}
		refresh = data(t, w)["refresh_token"].(string)
	}
	for _, p := range []string{devPath, "/api/v1/auth/login/apple", "/api/v1/auth/login/google"} {
		w := call(a.r, "POST", p, `{"email":"a@local","id_token":"x"}`, "")
		if w.Code != 429 || errCode(t, w) != "RATE_LIMITED" {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
		if secs, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || secs < 1 || secs > 60 {
			t.Fatalf("%s Retry-After = %q", p, w.Header().Get("Retry-After"))
		}
	}
	// refresh and logout are not limited.
	for i := 0; i < 3; i++ {
		w := call(a.r, "POST", "/api/v1/auth/refresh", `{"refresh_token":"`+refresh+`"}`, "")
		if w.Code != 200 {
			t.Fatalf("refresh %d: %d %s", i, w.Code, w.Body.String())
		}
		refresh = data(t, w)["refresh_token"].(string)
	}
	if w := call(a.r, "POST", "/api/v1/auth/logout", `{"refresh_token":"`+refresh+`"}`, ""); w.Code != 204 {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
}

func TestLogin_NoLimitWithoutWithLoginLimit(t *testing.T) {
	e := newEnv(t, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
	for i := 0; i < 12; i++ {
		if w := e.do("POST", devPath, `{"email":"a@local"}`); w.Code != 200 {
			t.Fatalf("login %d: %d", i, w.Code)
		}
	}
}

func TestTokensNeverLogged(t *testing.T) {
	e := newEnv(t, true, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
	access, refresh, _ := login(t, e, "a@local")
	w := e.do("POST", "/api/v1/auth/refresh", `{"refresh_token":"`+refresh+`"}`)
	refresh2 := data(t, w)["refresh_token"].(string)
	for _, secret := range []string{access, refresh, refresh2, testKey} {
		if strings.Contains(e.logs.String(), secret) {
			t.Fatalf("secret found in logs")
		}
	}
}
