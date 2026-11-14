package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/model"
)

// Recovery turns a panic into a logged 500 INTERNAL envelope.
func Recovery(l *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				l.Error("panic recovered", "panic", r, "stack", string(debug.Stack()),
					"request_id", handler.RequestIDFrom(c))
				handler.Fail(c, http.StatusInternalServerError, model.CodeInternal, "internal error")
			}
		}()
		c.Next()
	}
}
