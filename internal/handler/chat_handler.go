package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/service"
)

// ChatHandler serves sending and listing room messages (backend TRD 7.4).
type ChatHandler struct {
	chat   *service.ChatService
	authn  gin.HandlerFunc
	limits []gin.HandlerFunc
}

// NewChatHandler builds the handler; limits run after authn on the send route only.
func NewChatHandler(chat *service.ChatService, authn gin.HandlerFunc, limits ...gin.HandlerFunc) *ChatHandler {
	return &ChatHandler{chat: chat, authn: authn, limits: limits}
}

// Routes implements server.Module.
func (h *ChatHandler) Routes(v1 *gin.RouterGroup) {
	g := v1.Group("/rooms/:id/messages", h.authn)
	send := append(append([]gin.HandlerFunc{}, h.limits...), h.send)
	g.POST("", send...)
	g.GET("", h.list)
}

func (h *ChatHandler) send(c *gin.Context) {
	var in struct {
		ClientMsgID string `json:"client_msg_id"`
		Kind        string `json:"kind"`
		Text        string `json:"text"`
		PhraseID    string `json:"phrase_id"`
		Scope       string `json:"scope"`
		ToUserID    string `json:"to_user_id"`
		Polish      bool   `json:"polish"`
	}
	if !BindJSON(c, &in) {
		return
	}
	v, err := h.chat.Send(c.Request.Context(), UserID(c), c.Param("id"), service.SendInput{
		ClientMsgID: in.ClientMsgID, Kind: in.Kind, Text: in.Text, PhraseID: in.PhraseID,
		Scope: in.Scope, ToUserID: in.ToUserID, Polish: in.Polish,
	})
	if err != nil {
		failChat(c, err)
		return
	}
	OK(c, http.StatusAccepted, v)
}

func (h *ChatHandler) list(c *gin.Context) {
	q := model.PageQuery{Before: c.Query("before"), After: c.Query("after")}
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			failChat(c, &service.ValidationError{Fields: []string{"limit"}})
			return
		}
		q.Limit = n
	}
	page, err := h.chat.List(c.Request.Context(), UserID(c), c.Param("id"), q)
	if err != nil {
		failChat(c, err)
		return
	}
	if page.Items == nil {
		page.Items = []model.MessageView{}
	}
	OK(c, http.StatusOK, page)
}

// failChat maps validation errors to 400 with details.fields; the rest go to failRoom.
func failChat(c *gin.Context, err error) {
	var ve *service.ValidationError
	if errors.As(err, &ve) {
		FailDetails(c, http.StatusBadRequest, model.CodeValidationFailed, "invalid fields", gin.H{"fields": ve.Fields})
		return
	}
	failRoom(c, err)
}
