package user

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/auth"
	"github.com/jinheng0301/petbridge/internal/health"
	"github.com/jinheng0301/petbridge/internal/httpx"
	"github.com/jinheng0301/petbridge/internal/platform/db/dbtest"
	"github.com/jinheng0301/petbridge/internal/server"
)

func init() { gin.SetMode(gin.TestMode) }

func newRouter(t *testing.T) (*gin.Engine, func(q string, args ...any)) {
	t.Helper()
	pool := dbtest.Reset(t)
	iss := auth.NewIssuer("test-signing-key-test-signing-key", 15*time.Minute, time.Now)
	authSvc := auth.NewService(pool, iss, time.Hour)
	probe := func(context.Context) error { return nil }
	r := server.NewRouter(httpx.NewLogger("debug", &bytes.Buffer{}), health.New(probe, probe),
		auth.NewHandler(authSvc, true, auth.NotConfiguredVerifier{}, auth.NotConfiguredVerifier{}),
		NewHandler(NewService(pool, 7, time.Now), auth.Middleware(iss)))
	exec := func(q string, args ...any) {
		if _, err := pool.Exec(bg, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	return r, exec
}

func call(r *gin.Engine, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func asMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return m
}

func devLogin(t *testing.T, r *gin.Engine, email string) (token, uid string) {
	t.Helper()
	w := call(r, "POST", "/api/v1/auth/login/dev", `{"email":"`+email+`"}`, "")
	d := asMap(t, w)["data"].(map[string]any)
	return d["access_token"].(string), d["user"].(map[string]any)["id"].(string)
}

const onboardBody = `{"native_lang":"ja","mbti":"ENFP","nickname":"Mimi","age_band":"18_plus","tz":"Asia/Tokyo"}`

func TestMe_RequiresAuth(t *testing.T) {
	r, _ := newRouter(t)
	for _, p := range [][2]string{{"GET", "/api/v1/me"}, {"POST", "/api/v1/me/onboarding"}} {
		if w := call(r, p[0], p[1], onboardBody, ""); w.Code != 401 {
			t.Errorf("%s %s: %d", p[0], p[1], w.Code)
		}
	}
}

func TestOnboarding_HTTPFlow(t *testing.T) {
	r, _ := newRouter(t)
	tok, _ := devLogin(t, r, "a@local")

	me := asMap(t, call(r, "GET", "/api/v1/me", "", tok))["data"].(map[string]any)
	if me["onboarded"] != false || me["mbti"] != nil {
		t.Fatalf("before: %v", me)
	}

	w := call(r, "POST", "/api/v1/me/onboarding", onboardBody, tok)
	if w.Code != 200 {
		t.Fatalf("onboarding: %d %s", w.Code, w.Body.String())
	}
	d := asMap(t, w)["data"].(map[string]any)
	code := d["invite_code"].(map[string]any)["code"].(string)
	if len(code) != 8 || strings.Contains(code, "-") ||
		d["pet"].(map[string]any)["type"] != "happy_shiba" ||
		d["room"].(map[string]any)["member_count"] != float64(1) ||
		d["user"].(map[string]any)["onboarded"] != true {
		t.Fatalf("data = %v", d)
	}

	me = asMap(t, call(r, "GET", "/api/v1/me", "", tok))["data"].(map[string]any)
	if me["onboarded"] != true || me["pet_type"] != "happy_shiba" || me["nickname"] != "Mimi" {
		t.Fatalf("after: %v", me)
	}
}

func TestOnboarding_ValidationErrorListsFields(t *testing.T) {
	r, _ := newRouter(t)
	tok, _ := devLogin(t, r, "a@local")
	w := call(r, "POST", "/api/v1/me/onboarding", `{"native_lang":"xx","mbti":"enfp","nickname":"","age_band":"18_plus","tz":"Local"}`, tok)
	e := asMap(t, w)["error"].(map[string]any)
	fields, _ := e["details"].(map[string]any)["fields"].([]any)
	if w.Code != 400 || e["code"] != "VALIDATION_FAILED" || len(fields) != 4 {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestOnboarding_BadBody(t *testing.T) {
	r, _ := newRouter(t)
	tok, _ := devLogin(t, r, "a@local")
	for name, body := range map[string]string{
		"not json":  "not json",
		"oversized": `{"nickname":"` + strings.Repeat("a", 70000) + `"}`,
		"wrong typ": `{"mbti":5}`,
		"empty":     "",
	} {
		w := call(r, "POST", "/api/v1/me/onboarding", body, tok)
		if w.Code != 400 {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
}

func TestDisabledUserWithValidToken(t *testing.T) {
	r, exec := newRouter(t)
	tok, uid := devLogin(t, r, "a@local")
	exec(`update users set status='banned' where id=$1`, uid)
	for _, p := range [][2]string{{"GET", "/api/v1/me"}, {"POST", "/api/v1/me/onboarding"}} {
		w := call(r, p[0], p[1], onboardBody, tok)
		e, _ := asMap(t, w)["error"].(map[string]any)
		if w.Code != 403 || e["code"] != "ACCOUNT_DISABLED" {
			t.Errorf("%s %s: %d %s", p[0], p[1], w.Code, w.Body.String())
		}
	}
}
