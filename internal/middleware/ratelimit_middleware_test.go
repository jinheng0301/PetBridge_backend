package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
)

type brokenCounter struct{}

func (brokenCounter) Incr(context.Context, string, time.Duration) (ratelimit.Hit, error) {
	return ratelimit.Hit{}, errors.New("boom")
}
func (brokenCounter) SetFlag(context.Context, string, time.Duration) error { return nil }
func (brokenCounter) FlagTTL(context.Context, string) (time.Duration, error) {
	return 0, nil
}
func (brokenCounter) Delete(context.Context, string) error { return nil }

func rateLimitRouter(c ratelimit.Counter, limit int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/login", RateLimitIP(c, "login", limit, time.Minute), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": "ok"})
	})
	return r
}

func postLogin(r *gin.Engine) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	r.ServeHTTP(w, req)
	return w
}

func TestRateLimitIP_BlocksEleventh(t *testing.T) {
	clk := newClock()
	r := rateLimitRouter(ratelimit.NewMemory(clk.now), 10)
	for i := 1; i <= 10; i++ {
		if w := postLogin(r); w.Code != http.StatusOK {
			t.Fatalf("request %d = %d", i, w.Code)
		}
	}
	w := postLogin(r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th = %d, want 429", w.Code)
	}
	ra, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || ra <= 0 {
		t.Fatalf("Retry-After = %q", w.Header().Get("Retry-After"))
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != model.CodeRateLimited {
		t.Fatalf("code = %q", body.Error.Code)
	}
}

func TestRateLimitIP_FailsOpenOnCounterError(t *testing.T) {
	r := rateLimitRouter(brokenCounter{}, 1)
	for i := 0; i < 3; i++ {
		if w := postLogin(r); w.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 (fail open)", i, w.Code)
		}
	}
}

func TestRateLimitUser_PerUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	c := ratelimit.NewMemory(time.Now)
	r.GET("/ws", func(ctx *gin.Context) {
		ctx.Set(model.CtxUserID, ctx.GetHeader("X-User"))
		ctx.Next()
	}, RateLimitUser(c, "ws", 2, time.Minute), func(ctx *gin.Context) { ctx.Status(http.StatusOK) })
	get := func(user string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/ws", nil)
		req.Header.Set("X-User", user)
		r.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < 2; i++ {
		if w := get("u1"); w.Code != 200 {
			t.Fatalf("request %d: %d", i+1, w.Code)
		}
	}
	w := get("u1")
	var body struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusTooManyRequests || body.Error.Code != model.CodeRateLimited || w.Header().Get("Retry-After") == "" {
		t.Fatalf("third request: %d %s", w.Code, w.Body.String())
	}
	if w := get("u2"); w.Code != 200 {
		t.Fatalf("another user must have its own budget, got %d", w.Code)
	}
}
