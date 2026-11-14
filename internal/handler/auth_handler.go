package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/service"
	"github.com/jinheng0301/petbridge/pkg/idtoken"
)

const (
	maxEmailLen    = 254
	maxIDTokenLen  = 8192
	maxDeviceIDLen = 128
)

// AuthHandler serves the /auth routes.
type AuthHandler struct {
	svc         *service.AuthService
	devLogin    bool
	apple, goog idtoken.Verifier
}

func NewAuthHandler(svc *service.AuthService, devLogin bool, apple, google idtoken.Verifier) *AuthHandler {
	return &AuthHandler{svc: svc, devLogin: devLogin, apple: apple, goog: google}
}

// Routes implements server.Module. The dev route only exists when enabled.
func (h *AuthHandler) Routes(v1 *gin.RouterGroup) {
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

func (h *AuthHandler) loginDev(c *gin.Context) {
	var in struct {
		Email string `json:"email"`
	}
	if !BindJSON(c, &in) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" || len(email) > maxEmailLen || !strings.Contains(email, "@") {
		Fail(c, http.StatusBadRequest, model.CodeValidationFailed, "invalid email")
		return
	}
	res, err := h.svc.LoginWithIdentity(c.Request.Context(), "dev", email, deviceID(c))
	h.respond(c, res, err)
}

func (h *AuthHandler) loginWith(provider string, v idtoken.Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		var in struct {
			IDToken string `json:"id_token"`
		}
		if !BindJSON(c, &in) {
			return
		}
		if in.IDToken == "" || len(in.IDToken) > maxIDTokenLen {
			Fail(c, http.StatusBadRequest, model.CodeValidationFailed, "invalid id_token")
			return
		}
		sub, err := v.Verify(c.Request.Context(), in.IDToken)
		if errors.Is(err, idtoken.ErrProviderNotConfigured) {
			Fail(c, http.StatusNotImplemented, model.CodeProviderNotConfigured, provider+" login is not configured")
			return
		}
		if err != nil || sub == "" {
			Fail(c, http.StatusUnauthorized, model.CodeUnauthorized, "invalid id token")
			return
		}
		res, err := h.svc.LoginWithIdentity(c.Request.Context(), provider, sub, deviceID(c))
		h.respond(c, res, err)
	}
}

type refreshBody struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *AuthHandler) refresh(c *gin.Context) {
	var in refreshBody
	if !BindJSON(c, &in) {
		return
	}
	if in.RefreshToken == "" {
		Fail(c, http.StatusBadRequest, model.CodeValidationFailed, "refresh_token is required")
		return
	}
	res, err := h.svc.Refresh(c.Request.Context(), in.RefreshToken)
	h.respond(c, res, err)
}

func (h *AuthHandler) logout(c *gin.Context) {
	var in refreshBody
	if !BindJSON(c, &in) {
		return
	}
	if in.RefreshToken == "" {
		Fail(c, http.StatusBadRequest, model.CodeValidationFailed, "refresh_token is required")
		return
	}
	if err := h.svc.Logout(c.Request.Context(), in.RefreshToken); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *AuthHandler) respond(c *gin.Context, res model.LoginResult, err error) {
	if err != nil {
		h.fail(c, err)
		return
	}
	OK(c, http.StatusOK, gin.H{
		"access_token":  res.AccessToken,
		"refresh_token": res.RefreshToken,
		"expires_in":    res.ExpiresIn,
		"user":          gin.H{"id": res.UserID, "onboarded": res.Onboarded},
	})
}

func (h *AuthHandler) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrAccountDisabled):
		Fail(c, http.StatusForbidden, model.CodeAccountDisabled, "account disabled")
	case errors.Is(err, service.ErrRefreshReused):
		Fail(c, http.StatusUnauthorized, model.CodeRefreshReused, "refresh token reused")
	case errors.Is(err, service.ErrRefreshInvalid):
		Fail(c, http.StatusUnauthorized, model.CodeRefreshInvalid, "refresh token invalid")
	default:
		_ = c.Error(err) // logged by AccessLog; never echoed to the client
		Fail(c, http.StatusInternalServerError, model.CodeInternal, "internal error")
	}
}
