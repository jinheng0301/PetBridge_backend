package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
)

func init() { gin.SetMode(gin.TestMode) }

func quietLogger() *bytes.Buffer { return &bytes.Buffer{} }

func okProbe(context.Context) error { return nil }

func TestRouter_RequestIDOnEveryResponse(t *testing.T) {
	r := NewRouter(logger.New("debug", quietLogger()), handler.NewHealthHandler(okProbe, okProbe))
	for _, path := range []string{"/healthz", "/readyz", "/nope"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Header().Get("X-Request-ID") == "" {
			t.Errorf("%s: missing X-Request-ID (status %d)", path, w.Code)
		}
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestRun_GracefulShutdown(t *testing.T) {
	addr := freeAddr(t)
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, addr, slow, logger.New("debug", quietLogger())) }()

	// wait until the server accepts connections
	for i := 0; i < 50; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	type result struct {
		code int
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		resp.Body.Close()
		resCh <- result{code: resp.StatusCode}
	}()
	time.Sleep(100 * time.Millisecond) // request is now in flight
	cancel()

	res := <-resCh
	if res.err != nil || res.code != 200 {
		t.Fatalf("in-flight request: code=%d err=%v", res.code, res.err)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}

func TestRun_PortInUse_ReturnsError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), l.Addr().String(), http.NewServeMux(), logger.New("debug", quietLogger()))
	}()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("want listen error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run blocked on an occupied port")
	}
}
