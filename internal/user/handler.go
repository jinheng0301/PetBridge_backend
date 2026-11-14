package user

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/auth"
	"github.com/jinheng0301/petbridge/internal/httpx"
)

// Handler serves GET /me and POST /me/onboarding.
type Handler struct {
	svc   *Service
	authn gin.HandlerFunc
}

func NewHandler(svc *Service, authn gin.HandlerFunc) *Handler {
	return &Handler{svc: svc, authn: authn}
}

// Routes implements server.Module.
func (h *Handler) Routes(v1 *gin.RouterGroup) {
	g := v1.Group("/me", h.authn)
	g.GET("", h.me)
	g.POST("/onboarding", h.onboarding)
}

func (h *Handler) me(c *gin.Context) {
	p, err := h.svc.Me(c.Request.Context(), auth.UserID(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, p)
}

func (h *Handler) onboarding(c *gin.Context) {
	var in OnboardingInput
	if !httpx.BindJSON(c, &in) {
		return
	}
	res, err := h.svc.Onboard(c.Request.Context(), auth.UserID(c), in)
	if err != nil {
		h.fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, res)
}

func (h *Handler) fail(c *gin.Context, err error) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		httpx.FailDetails(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid fields", gin.H{"fields": ve.Fields})
	case errors.Is(err, ErrAccountDisabled):
		httpx.Fail(c, http.StatusForbidden, httpx.CodeAccountDisabled, "account disabled")
	case errors.Is(err, ErrNotFound):
		httpx.Fail(c, http.StatusUnauthorized, httpx.CodeUnauthorized, "unauthorized")
	default:
		_ = c.Error(err)
		httpx.Fail(c, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
	}
}
