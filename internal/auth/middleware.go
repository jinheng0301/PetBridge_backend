package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/httpx"
)

const userIDKey = "auth_user_id"

// Middleware requires "Authorization: Bearer <access token>". Every failure is
// the same 401 so callers learn nothing about why.
func Middleware(iss *Issuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			httpx.Fail(c, http.StatusUnauthorized, httpx.CodeUnauthorized, "unauthorized")
			return
		}
		uid, err := iss.Parse(parts[1])
		if err != nil {
			httpx.Fail(c, http.StatusUnauthorized, httpx.CodeUnauthorized, "unauthorized")
			return
		}
		c.Set(userIDKey, uid)
		c.Next()
	}
}

// UserID returns the authenticated user's id (empty if the route is unprotected).
func UserID(c *gin.Context) string { return c.GetString(userIDKey) }
