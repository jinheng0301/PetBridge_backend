package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/service"
)

// BlockHandler serves the caller's block list (spec 6.2).
type BlockHandler struct {
	svc   *service.BlockService
	authn gin.HandlerFunc
}

func NewBlockHandler(svc *service.BlockService, authn gin.HandlerFunc) *BlockHandler {
	return &BlockHandler{svc: svc, authn: authn}
}

// Routes implements server.Module.
func (h *BlockHandler) Routes(v1 *gin.RouterGroup) {
	g := v1.Group("/blocks", h.authn)
	g.POST("", h.block)
	g.DELETE("/:user_id", h.unblock)
	g.GET("", h.list)
}

func (h *BlockHandler) block(c *gin.Context) {
	var in struct {
		UserID string `json:"user_id"`
	}
	if !BindJSON(c, &in) {
		return
	}
	if err := h.svc.Block(c.Request.Context(), UserID(c), in.UserID); err != nil {
		failBlock(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *BlockHandler) unblock(c *gin.Context) {
	if err := h.svc.Unblock(c.Request.Context(), UserID(c), c.Param("user_id")); err != nil {
		failBlock(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *BlockHandler) list(c *gin.Context) {
	items, err := h.svc.List(c.Request.Context(), UserID(c))
	if err != nil {
		failBlock(c, err)
		return
	}
	OK(c, http.StatusOK, gin.H{"items": items})
}

func failBlock(c *gin.Context, err error) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		FailDetails(c, http.StatusBadRequest, model.CodeValidationFailed, "invalid fields", gin.H{"fields": ve.Fields})
	case errors.Is(err, service.ErrBlockLimit):
		Fail(c, http.StatusConflict, model.CodeBlockLimit, "block limit reached")
	default:
		failRoom(c, err)
	}
}
