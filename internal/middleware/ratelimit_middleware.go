package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
)

// RateLimitIP allows limit requests per window from one client IP, counted under
// rl:{name}:ip:{ip}. Over the limit it answers 429 RATE_LIMITED with Retry-After
// (whole seconds, rounded up). A counter error lets the request through and is
// recorded with c.Error: limiting is best effort and must not take login down.
func RateLimitIP(c ratelimit.Counter, name string, limit int, window time.Duration) gin.HandlerFunc {
	return func(ctx *gin.Context) {
		hit, err := c.Incr(ctx.Request.Context(), "rl:"+name+":ip:"+ctx.ClientIP(), window)
		if err != nil {
			_ = ctx.Error(err)
			ctx.Next()
			return
		}
		if hit.Count > int64(limit) {
			ctx.Header("Retry-After", strconv.Itoa(handler.RetrySeconds(hit.Resets)))
			handler.Fail(ctx, http.StatusTooManyRequests, model.CodeRateLimited, "too many requests")
			return
		}
		ctx.Next()
	}
}
