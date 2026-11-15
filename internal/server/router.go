// Package server assembles the HTTP router and runs the HTTP server.
package server

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/middleware"
)

// Module is a handler that mounts its routes under /api/v1.
type Module interface {
	Routes(v1 *gin.RouterGroup)
}

// NewRouter wires middleware (RequestID -> AccessLog -> Recovery), the probe
// routes and every module's routes under /api/v1.
func NewRouter(l *slog.Logger, h *handler.HealthHandler, mods ...Module) *gin.Engine {
	r := gin.New()
	// Trust no proxy: X-Forwarded-For is client-controlled, so ClientIP must be
	// the socket peer or per-IP rate limits can be bypassed. Behind Caddy, set
	// the proxy's address here at deploy time.
	_ = r.SetTrustedProxies(nil)
	r.Use(middleware.RequestID(), middleware.AccessLog(l), middleware.Recovery(l))
	r.GET("/healthz", h.Healthz)
	r.GET("/readyz", h.Readyz)
	v1 := r.Group("/api/v1")
	for _, m := range mods {
		m.Routes(v1)
	}
	return r
}
