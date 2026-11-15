package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/service"
)

// CodeHandler serves GET /codes.
type CodeHandler struct {
	codes *service.CodeService
	authn gin.HandlerFunc
}

func NewCodeHandler(codes *service.CodeService, authn gin.HandlerFunc) *CodeHandler {
	return &CodeHandler{codes: codes, authn: authn}
}

// Routes implements server.Module.
func (h *CodeHandler) Routes(v1 *gin.RouterGroup) {
	v1.GET("/codes", h.authn, h.list)
}

func (h *CodeHandler) list(c *gin.Context) {
	items, err := h.codes.List(c.Request.Context(), UserID(c))
	if err != nil {
		failRoom(c, err)
		return
	}
	OK(c, http.StatusOK, items)
}
