// Package handler is the HTTP/WebSocket access layer (Gin). It binds and
// validates input, calls services and shapes the response envelope; it holds
// no business rules.
package handler

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
)

// OK writes {"data": ...}.
func OK(c *gin.Context, status int, data any) {
	c.JSON(status, gin.H{"data": data})
}

// Fail writes {"error": {code, message, request_id}} and aborts the chain.
func Fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code":       code,
		"message":    message,
		"request_id": RequestIDFrom(c),
	}})
}

// FailDetails is Fail with an extra "details" object inside "error".
func FailDetails(c *gin.Context, status int, code, message string, details any) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"code":       code,
		"message":    message,
		"request_id": RequestIDFrom(c),
		"details":    details,
	}})
}

// RequestIDFrom returns the request id set by the RequestID middleware.
func RequestIDFrom(c *gin.Context) string { return c.GetString(model.CtxRequestID) }

// UserID returns the authenticated user's id (empty if the route is unprotected).
func UserID(c *gin.Context) string { return c.GetString(model.CtxUserID) }

// RetrySeconds turns a wait into whole seconds for Retry-After and
// retry_after_seconds: rounded up, never below 1.
func RetrySeconds(d time.Duration) int {
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return secs
}

// MaxBodyBytes caps every JSON request body.
const MaxBodyBytes = 64 << 10

// BindJSON decodes the request body into dst. A missing, oversized or
// malformed body writes 400 VALIDATION_FAILED and returns false.
func BindJSON(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)
	if err := json.NewDecoder(c.Request.Body).Decode(dst); err != nil {
		Fail(c, http.StatusBadRequest, model.CodeValidationFailed, "invalid request body")
		return false
	}
	return true
}
