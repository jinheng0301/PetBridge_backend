package middleware

import (
	"context"
	"errors"
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

// AccountChecker reports users.status ("active", "banned", "deleted").
// service.UserService satisfies it.
type AccountChecker interface {
	AccountStatus(ctx context.Context, userID string) (string, error)
}

// Auth requires "Authorization: Bearer <access token>". Every failure is the
// same 401 so callers learn nothing about why. On success the user id is
// stored under model.CtxUserID (read it with handler.UserID). The account must
// also be active: access tokens outlive a ban by up to their TTL, so the status
// is checked on every request (one indexed primary-key lookup).
func Auth(p TokenParser, a AccountChecker) gin.HandlerFunc {
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
		status, err := a.AccountStatus(c.Request.Context(), uid)
		switch {
		case errors.Is(err, model.ErrNotFound):
			handler.Fail(c, http.StatusUnauthorized, model.CodeUnauthorized, "unauthorized")
			return
		case err != nil:
			handler.Fail(c, http.StatusInternalServerError, model.CodeInternal, "internal error")
			return
		case status != "active":
			handler.Fail(c, http.StatusForbidden, model.CodeAccountDisabled, "account disabled")
			return
		}
		c.Set(model.CtxUserID, uid)
		c.Next()
	}
}
