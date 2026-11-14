package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/model"
)

// TokenParser verifies an access token and returns its subject (user id).
// service.Issuer satisfies it.
type TokenParser interface {
	Parse(token string) (string, error)
}

// Auth requires "Authorization: Bearer <access token>". Every failure is the
// same 401 so callers learn nothing about why. On success the user id is
// stored under model.CtxUserID (read it with handler.UserID).
func Auth(p TokenParser) gin.HandlerFunc {
	return func(c *gin.Context) {
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			handler.Fail(c, http.StatusUnauthorized, model.CodeUnauthorized, "unauthorized")
			return
		}
		uid, err := p.Parse(parts[1])
		if err != nil {
			handler.Fail(c, http.StatusUnauthorized, model.CodeUnauthorized, "unauthorized")
			return
		}
		c.Set(model.CtxUserID, uid)
		c.Next()
	}
}
