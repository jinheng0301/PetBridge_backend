package auth

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/httpx"
)

const (
	maxEmailLen    = 254
	maxIDTokenLen  = 8192
	maxDeviceIDLen = 128
)

// Handler serves the /auth routes.
type Handler struct {
	svc         *Service
	devLogin    bool
	apple, goog IDTokenVerifier
}

func NewHandler(svc *Service, devLogin bool, apple, google IDTokenVerifier) *Handler {
	return &Handler{svc: svc, devLogin: devLogin, apple: apple, goog: google}
}

// Routes implements server.Module. The dev route only exists when enabled.
func (h *Handler) Routes(v1 *gin.RouterGroup) {
	g := v1.Group("/auth")
	if h.devLogin {
		g.POST("/login/dev", h.loginDev)
	}
	g.POST("/login/apple", h.loginWith("apple", h.apple))
	g.POST("/login/google", h.loginWith("google", h.goog))
	g.POST("/refresh", h.refresh)
	g.POST("/logout", h.logout)
}

func deviceID(c *gin.Context) string {
	id := c.GetHeader("X-Device-ID")
	if len(id) > maxDeviceIDLen {
		return ""
	}
	return id
}

func (h *Handler) loginDev(c *gin.Context) {
	var in struct {
		Email string `json:"email"`
	}
	if !httpx.BindJSON(c, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" || len(email) > maxEmailLen || !strings.Contains(email, "@") {
		httpx.Fail(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid email")
		return
	}
	res, err := h.svc.LoginWithIdentity(c.Request.Context(), "dev", email, deviceID(c))
	h.respond(c, res, err)
}

func (h *Handler) loginWith(provider string, v IDTokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in struct {
			IDToken string `json:"id_token"`
		}
		if !httpx.BindJSON(c, &in) {
			return
		}
		if in.IDToken == "" || len(in.IDToken) > maxIDTokenLen {
			httpx.Fail(c, http.StatusBadRequest, httpx.CodeValidationFailed, "invalid id_token")
			return
		}
		sub, err := v.Verify(c.Request.Context(), in.IDToken)
		if errors.Is(err, ErrProviderNotConfigured) {
			httpx.Fail(c, http.StatusNotImplemented, httpx.CodeProviderNotConfigured, provider+" login is not configured")
			return
		}
		if err != nil || sub == "" {
			httpx.Fail(c, http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid id token")
			return
		}
		res, err := h.svc.LoginWithIdentity(c.Request.Context(), provider, sub, deviceID(c))
		h.respond(c, res, err)
	}
}

type refreshBody struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) refresh(c *gin.Context) {
	var in refreshBody
	if !httpx.BindJSON(c, &in) {
		return
	}
	if in.RefreshToken == "" {
		httpx.Fail(c, http.StatusBadRequest, httpx.CodeValidationFailed, "refresh_token is required")
		return
	}
	res, err := h.svc.Refresh(c.Request.Context(), in.RefreshToken)
	h.respond(c, res, err)
}

func (h *Handler) logout(c *gin.Context) {
	var in refreshBody
	if !httpx.BindJSON(c, &in) {
		return
	}
	if in.RefreshToken == "" {
		httpx.Fail(c, http.StatusBadRequest, httpx.CodeValidationFailed, "refresh_token is required")
		return
	}
	if err := h.svc.Logout(c.Request.Context(), in.RefreshToken); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) respond(c *gin.Context, res LoginResult, err error) {
	if err != nil {
		h.fail(c, err)
		return
	}
	httpx.OK(c, http.StatusOK, gin.H{
		"access_token":  res.AccessToken,
		"refresh_token": res.RefreshToken,
		"expires_in":    res.ExpiresIn,
		"user":          gin.H{"id": res.UserID, "onboarded": res.Onboarded},
	})
}

func (h *Handler) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrAccountDisabled):
		httpx.Fail(c, http.StatusForbidden, httpx.CodeAccountDisabled, "account disabled")
	case errors.Is(err, ErrRefreshReused):
		httpx.Fail(c, http.StatusUnauthorized, httpx.CodeRefreshReused, "refresh token reused")
	case errors.Is(err, ErrRefreshInvalid):
		httpx.Fail(c, http.StatusUnauthorized, httpx.CodeRefreshInvalid, "refresh token invalid")
	default:
		_ = c.Error(err) // logged by AccessLog; never echoed to the client
		httpx.Fail(c, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
	}
}
