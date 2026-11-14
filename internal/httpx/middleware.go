package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	requestIDHeader = "X-Request-ID"
	requestIDKey    = "request_id"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RequestID accepts a well-formed inbound X-Request-ID, otherwise generates
// one, so untrusted values never reach logs or response headers.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if !safeID.MatchString(id) {
			id = newID()
		}
		c.Set(requestIDKey, id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

func RequestIDFrom(c *gin.Context) string {
	return c.GetString(requestIDKey)
}

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
			"request_id", RequestIDFrom(c),
		}
		// Handlers attach unexpected internal errors with c.Error(err).
		if len(c.Errors) > 0 {
			attrs = append(attrs, "error", c.Errors.String())
		}
		l.Info("request", attrs...)
	}
}

// Recovery turns a panic into a logged 500 INTERNAL envelope.
func Recovery(l *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				l.Error("panic recovered", "panic", r, "stack", string(debug.Stack()),
					"request_id", RequestIDFrom(c))
				Fail(c, http.StatusInternalServerError, CodeInternal, "internal error")
			}
		}()
		c.Next()
	}
}
