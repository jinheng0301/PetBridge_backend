package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
)

const probeTimeout = 2 * time.Second

// Probe checks one dependency.
type Probe func(ctx context.Context) error

// HealthHandler serves the liveness and readiness probes.
type HealthHandler struct {
	db, redis Probe
}

func NewHealthHandler(dbProbe, redisProbe Probe) *HealthHandler {
	return &HealthHandler{db: dbProbe, redis: redisProbe}
}

// Healthz reports that the process is alive; it checks no dependencies.
func (h *HealthHandler) Healthz(c *gin.Context) {
	OK(c, http.StatusOK, gin.H{"status": "ok"})
}

// Readyz: PostgreSQL down -> 503; Redis down -> 200 "degraded" (Redis failure
// must not stop message delivery, see TRD 12.2); both up -> 200 "ok".
func (h *HealthHandler) Readyz(c *gin.Context) {
	if err := run(c.Request.Context(), h.db); err != nil {
		Fail(c, http.StatusServiceUnavailable, model.CodeNotReady, "database unavailable")
		return
	}
	if err := run(c.Request.Context(), h.redis); err != nil {
		OK(c, http.StatusOK, gin.H{"status": "degraded", "redis": "down"})
		return
	}
	OK(c, http.StatusOK, gin.H{"status": "ok"})
}

func run(ctx context.Context, p Probe) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return p(ctx)
}
