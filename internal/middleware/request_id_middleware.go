// Package middleware holds the Gin middleware chain: request id, logging,
// panic recovery and authentication.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
)

const requestIDHeader = "X-Request-ID"

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
		c.Set(model.CtxRequestID, id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}
