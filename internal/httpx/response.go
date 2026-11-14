package httpx

import "github.com/gin-gonic/gin"

const (
	CodeInternal = "INTERNAL"
	CodeNotReady = "NOT_READY"
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
