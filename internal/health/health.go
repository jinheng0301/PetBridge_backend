// Package health implements liveness and readiness endpoints.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/httpx"
)

const probeTimeout = 2 * time.Second

// Probe checks one dependency.
type Probe func(ctx context.Context) error

type Handler struct {
	db, redis Probe
}

func New(dbProbe, redisProbe Probe) *Handler {
	return &Handler{db: dbProbe, redis: redisProbe}
}

// Healthz reports that the process is alive; it checks no dependencies.
func (h *Handler) Healthz(c *gin.Context) {
	httpx.OK(c, http.StatusOK, gin.H{"status": "ok"})
}

// Readyz: PostgreSQL down -> 503; Redis down -> 200 "degraded" (Redis failure
// must not stop message delivery, see TRD 12.2); both up -> 200 "ok".
func (h *Handler) Readyz(c *gin.Context) {
	if err := run(c.Request.Context(), h.db); err != nil {
		httpx.Fail(c, http.StatusServiceUnavailable, httpx.CodeNotReady, "database unavailable")
		return
	}
	if err := run(c.Request.Context(), h.redis); err != nil {
		httpx.OK(c, http.StatusOK, gin.H{"status": "degraded", "redis": "down"})
		return
	}
	httpx.OK(c, http.StatusOK, gin.H{"status": "ok"})
}

func run(ctx context.Context, p Probe) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return p(ctx)
}
