package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/service"
)

// ReportHandler accepts user reports (spec 7).
type ReportHandler struct {
	svc   *service.ReportService
	authn gin.HandlerFunc
}

func NewReportHandler(svc *service.ReportService, authn gin.HandlerFunc) *ReportHandler {
	return &ReportHandler{svc: svc, authn: authn}
}

// Routes implements server.Module.
func (h *ReportHandler) Routes(v1 *gin.RouterGroup) {
	v1.POST("/reports", h.authn, h.report)
}

func (h *ReportHandler) report(c *gin.Context) {
	var in struct {
		TargetUserID string `json:"target_user_id"`
		RoomID       string `json:"room_id"`
		MessageID    string `json:"message_id"`
		Reason       string `json:"reason"`
		Note         string `json:"note"`
	}
	if !BindJSON(c, &in) {
		return
	}
	res, err := h.svc.Report(c.Request.Context(), UserID(c), service.ReportInput{
		TargetUserID: in.TargetUserID, RoomID: in.RoomID, MessageID: in.MessageID, Reason: in.Reason, Note: in.Note,
	})
	if err != nil {
		var ve *service.ValidationError
		if errors.As(err, &ve) {
			FailDetails(c, http.StatusBadRequest, model.CodeValidationFailed, "invalid fields", gin.H{"fields": ve.Fields})
			return
		}
		failRoom(c, err)
		return
	}
	OK(c, http.StatusCreated, res)
}
