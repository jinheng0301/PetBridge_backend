package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/service"
)

// WithManage enables the owner-management and leave routes (B3b).
func (h *RoomHandler) WithManage(m *service.ManageService) *RoomHandler {
	h.manage = m
	return h
}

func (h *RoomHandler) removeMember(c *gin.Context) {
	room, err := h.manage.Remove(c.Request.Context(), UserID(c), c.Param("id"), c.Param("user_id"))
	if err != nil {
		failRoom(c, err)
		return
	}
	OK(c, http.StatusOK, model.RoomResult{Room: room})
}

func (h *RoomHandler) dissolve(c *gin.Context) {
	room, err := h.manage.Dissolve(c.Request.Context(), UserID(c), c.Param("id"))
	if err != nil {
		failRoom(c, err)
		return
	}
	OK(c, http.StatusOK, model.RoomResult{Room: room})
}

func (h *RoomHandler) regenCode(c *gin.Context) {
	code, err := h.manage.RegenCode(c.Request.Context(), UserID(c), c.Param("id"))
	if err != nil {
		failRoom(c, err)
		return
	}
	OK(c, http.StatusCreated, code)
}

func (h *RoomHandler) leave(c *gin.Context) {
	if err := h.manage.Leave(c.Request.Context(), UserID(c), c.Param("id")); err != nil {
		failRoom(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
