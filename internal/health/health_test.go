package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/httpx"
)

func init() { gin.SetMode(gin.TestMode) }

var (
	up   Probe = func(context.Context) error { return nil }
	down Probe = func(context.Context) error { return errors.New("down") }
)

func call(h *Handler, path string) *httptest.ResponseRecorder {
	r := gin.New()
	r.Use(httpx.RequestID())
	r.GET("/healthz", h.Healthz)
	r.GET("/readyz", h.Readyz)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func data(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var b struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	return b.Data
}

func TestHealthz_AlwaysOK(t *testing.T) {
	w := call(New(down, down), "/healthz")
	if w.Code != 200 || data(t, w)["status"] != "ok" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReadyz_AllUp(t *testing.T) {
	w := call(New(up, up), "/readyz")
	if w.Code != 200 || data(t, w)["status"] != "ok" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReadyz_RedisDown_Degraded(t *testing.T) {
	w := call(New(up, down), "/readyz")
	d := data(t, w)
	if w.Code != 200 || d["status"] != "degraded" || d["redis"] != "down" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReadyz_DBDown_503(t *testing.T) {
	w := call(New(down, up), "/readyz")
	var b struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	if w.Code != 503 || b.Error.Code != "NOT_READY" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReadyz_ProbeTimeout(t *testing.T) {
	block := Probe(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	start := time.Now()
	w := call(New(block, block), "/readyz")
	if time.Since(start) > 3*time.Second {
		t.Fatalf("readyz took %v", time.Since(start))
	}
	if w.Code != 503 {
		t.Fatalf("want 503 when db probe times out, got %d", w.Code)
	}
}
