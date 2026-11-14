// Package server assembles the HTTP router and runs the HTTP server.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/health"
	"github.com/jinheng0301/petbridge/internal/httpx"
)

const shutdownTimeout = 10 * time.Second

// Module is a business package that mounts its routes under /api/v1.
type Module interface {
	Routes(v1 *gin.RouterGroup)
}

// NewRouter wires middleware (RequestID -> AccessLog -> Recovery), the probe
// routes and every module's routes under /api/v1.
func NewRouter(l *slog.Logger, h *health.Handler, mods ...Module) *gin.Engine {
	r := gin.New()
	r.Use(httpx.RequestID(), httpx.AccessLog(l), httpx.Recovery(l))
	r.GET("/healthz", h.Healthz)
	r.GET("/readyz", h.Readyz)
	v1 := r.Group("/api/v1")
	for _, m := range mods {
		m.Routes(v1)
	}
	return r
}

// Run serves until ctx is cancelled, then shuts down gracefully (in-flight
// requests get up to 10 seconds). It returns an error if the address cannot
// be bound (for example, port already in use).
func Run(ctx context.Context, addr string, handler http.Handler, l *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	l.Info("http server listening", "addr", addr)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	l.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return err
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
