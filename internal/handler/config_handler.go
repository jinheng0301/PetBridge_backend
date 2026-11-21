package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/service"
)

// ConfigHandler serves client-cached configuration, currently the phrase book.
type ConfigHandler struct {
	authn gin.HandlerFunc
}

func NewConfigHandler(authn gin.HandlerFunc) *ConfigHandler {
	return &ConfigHandler{authn: authn}
}

// Routes implements server.Module.
func (h *ConfigHandler) Routes(v1 *gin.RouterGroup) {
	v1.GET("/config/phrases", h.authn, h.phrases)
}

func (h *ConfigHandler) phrases(c *gin.Context) {
	if c.Query("v") == service.PhrasesVersion {
		c.Status(http.StatusNotModified)
		return
	}
	OK(c, http.StatusOK, gin.H{"version": service.PhrasesVersion, "phrases": service.Phrases()})
}
