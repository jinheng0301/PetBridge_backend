package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/service"
)

// UserHandler serves GET /me and POST /me/onboarding.
type UserHandler struct {
	svc   *service.UserService
	authn gin.HandlerFunc
}

func NewUserHandler(svc *service.UserService, authn gin.HandlerFunc) *UserHandler {
	return &UserHandler{svc: svc, authn: authn}
}

// Routes implements server.Module.
func (h *UserHandler) Routes(v1 *gin.RouterGroup) {
	g := v1.Group("/me", h.authn)
	g.GET("", h.me)
	g.POST("/onboarding", h.onboarding)
}

func (h *UserHandler) me(c *gin.Context) {
	p, err := h.svc.Me(c.Request.Context(), UserID(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	OK(c, http.StatusOK, p)
}

func (h *UserHandler) onboarding(c *gin.Context) {
	var in model.OnboardingInput
	if !BindJSON(c, &in) {
		return
	}
	res, err := h.svc.Onboard(c.Request.Context(), UserID(c), in)
	if err != nil {
		h.fail(c, err)
		return
	}
	OK(c, http.StatusOK, res)
}

func (h *UserHandler) fail(c *gin.Context, err error) {
	var ve *service.ValidationError
	var cb *service.ContentBlockedError
	switch {
	case errors.As(err, &ve):
		FailDetails(c, http.StatusBadRequest, model.CodeValidationFailed, "invalid fields", gin.H{"fields": ve.Fields})
	case errors.As(err, &cb):
		failContentBlocked(c, cb)
	case errors.Is(err, service.ErrAccountDisabled):
		Fail(c, http.StatusForbidden, model.CodeAccountDisabled, "account disabled")
	case errors.Is(err, service.ErrUserNotFound):
		Fail(c, http.StatusUnauthorized, model.CodeUnauthorized, "unauthorized")
	default:
		_ = c.Error(err)
		Fail(c, http.StatusInternalServerError, model.CodeInternal, "internal error")
	}
}
