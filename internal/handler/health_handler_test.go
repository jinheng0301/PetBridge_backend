package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/middleware"
)

var (
	up   handler.Probe = func(context.Context) error { return nil }
	down handler.Probe = func(context.Context) error { return errors.New("down") }
)

func callHealth(h *handler.HealthHandler, path string) *httptest.ResponseRecorder {
	r := gin.New()
	r.Use(middleware.RequestID())
	r.GET("/healthz", h.Healthz)
	r.GET("/readyz", h.Readyz)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func healthData(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
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
	w := callHealth(handler.NewHealthHandler(down, down), "/healthz")
	if w.Code != 200 || healthData(t, w)["status"] != "ok" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReadyz_AllUp(t *testing.T) {
	w := callHealth(handler.NewHealthHandler(up, up), "/readyz")
	if w.Code != 200 || healthData(t, w)["status"] != "ok" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReadyz_RedisDown_Degraded(t *testing.T) {
	w := callHealth(handler.NewHealthHandler(up, down), "/readyz")
	d := healthData(t, w)
	if w.Code != 200 || d["status"] != "degraded" || d["redis"] != "down" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReadyz_DBDown_503(t *testing.T) {
	w := callHealth(handler.NewHealthHandler(down, up), "/readyz")
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
	block := handler.Probe(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	start := time.Now()
	w := callHealth(handler.NewHealthHandler(block, block), "/readyz")
	if time.Since(start) > 3*time.Second {
		t.Fatalf("readyz took %v", time.Since(start))
	}
	if w.Code != 503 {
		t.Fatalf("want 503 when db probe times out, got %d", w.Code)
	}
}
