package handler

import (
	"context"
	"fmt"
	"net/http"
	"sync"
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
	// Probe both at once so two slow dependencies cost one timeout, not two.
	var dbErr, redisErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); dbErr = run(c.Request.Context(), h.db) }()
	go func() { defer wg.Done(); redisErr = run(c.Request.Context(), h.redis) }()
	wg.Wait()

	// c.Error keeps the cause in the access log; it is never sent to the client.
	if dbErr != nil {
		_ = c.Error(fmt.Errorf("readyz: database probe: %w", dbErr))
		Fail(c, http.StatusServiceUnavailable, model.CodeNotReady, "database unavailable")
		return
	}
	if redisErr != nil {
		_ = c.Error(fmt.Errorf("readyz: redis probe: %w", redisErr))
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
