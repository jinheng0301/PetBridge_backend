package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/service"
)

// CheckinHandler serves daily check-ins and a room's decorations (B6b spec 8).
type CheckinHandler struct {
	svc    *service.CheckinService
	authn  gin.HandlerFunc
	limits []gin.HandlerFunc
}

// NewCheckinHandler builds the handler; limits run after authn on the check-in route only.
func NewCheckinHandler(svc *service.CheckinService, authn gin.HandlerFunc, limits ...gin.HandlerFunc) *CheckinHandler {
	return &CheckinHandler{svc: svc, authn: authn, limits: limits}
}

// Routes implements server.Module.
func (h *CheckinHandler) Routes(v1 *gin.RouterGroup) {
	g := v1.Group("/rooms/:id", h.authn)
	g.POST("/checkin", append(append([]gin.HandlerFunc{}, h.limits...), h.checkin)...)
	g.GET("/decor", h.decor)
}

func (h *CheckinHandler) checkin(c *gin.Context) {
	res, err := h.svc.Checkin(c.Request.Context(), UserID(c), c.Param("id"))
	if err != nil {
		failRoom(c, err)
		return
	}
	OK(c, http.StatusOK, res)
}

func (h *CheckinHandler) decor(c *gin.Context) {
	list, err := h.svc.Decor(c.Request.Context(), UserID(c), c.Param("id"))
	if err != nil {
		failRoom(c, err)
		return
	}
	OK(c, http.StatusOK, list)
}
