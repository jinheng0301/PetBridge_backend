package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
)

// AccessLog logs one line per request: no query string, no body.
func AccessLog(l *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		attrs := []any{
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", handler.RequestIDFrom(c),
		}
		// Handlers attach unexpected internal errors with c.Error(err).
		if len(c.Errors) > 0 {
			attrs = append(attrs, "error", c.Errors.String())
		}
		l.Info("request", attrs...)
	}
}
