package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

const (
	CodeInternal = "INTERNAL"
	CodeNotReady = "NOT_READY"

	CodeUnauthorized          = "UNAUTHORIZED"
	CodeValidationFailed      = "VALIDATION_FAILED"
	CodeAccountDisabled       = "ACCOUNT_DISABLED"
	CodeRefreshReused         = "REFRESH_REUSED"
	CodeRefreshInvalid        = "REFRESH_INVALID"
	CodeProviderNotConfigured = "PROVIDER_NOT_CONFIGURED"
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

// MaxBodyBytes caps every JSON request body.
const MaxBodyBytes = 64 << 10

// BindJSON decodes the request body into dst. A missing, oversized or
// malformed body writes 400 VALIDATION_FAILED and returns false.
func BindJSON(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes)
	if err := json.NewDecoder(c.Request.Body).Decode(dst); err != nil {
		Fail(c, http.StatusBadRequest, CodeValidationFailed, "invalid request body")
		return false
	}
	return true
}
