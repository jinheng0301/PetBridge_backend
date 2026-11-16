package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/jinheng0301/petbridge/internal/model"
	"github.com/jinheng0301/petbridge/internal/service"
)

// RoomHandler serves join, mine and (with WithManage) the owner-management routes.
type RoomHandler struct {
	join   *service.JoinService
	rooms  *service.RoomService
	manage *service.ManageService
	authn  gin.HandlerFunc
}

func NewRoomHandler(join *service.JoinService, rooms *service.RoomService, authn gin.HandlerFunc) *RoomHandler {
	return &RoomHandler{join: join, rooms: rooms, authn: authn}
}

// Routes implements server.Module.
func (h *RoomHandler) Routes(v1 *gin.RouterGroup) {
	g := v1.Group("/rooms", h.authn)
	g.POST("/join", h.joinRoom)
	g.GET("/mine", h.mine)
	g.DELETE("/:id/members/:user_id", h.removeMember)
	g.POST("/:id/dissolve", h.dissolve)
	g.POST("/:id/codes", h.regenCode)
	g.POST("/:id/leave", h.leave)
}

func (h *RoomHandler) joinRoom(c *gin.Context) {
	// A pointer tells a missing or null "code" (VALIDATION_FAILED, not counted
	// as a failure) from a present one, which Join judges however odd it is.
	var in struct {
		Code *string `json:"code"`
	}
	if !BindJSON(c, &in) {
		return
	}
	if in.Code == nil {
		Fail(c, http.StatusBadRequest, model.CodeValidationFailed, "code is required")
		return
	}
	res, err := h.join.Join(c.Request.Context(), UserID(c), c.ClientIP(), *in.Code)
	if err != nil {
		failRoom(c, err)
		return
	}
	if res.Members == nil {
		res.Members = []model.MemberInfo{}
	}
	if res.Rewards == nil {
		res.Rewards = []model.CodeInfo{}
	}
	OK(c, http.StatusOK, res)
}

func (h *RoomHandler) mine(c *gin.Context) {
	rooms, err := h.rooms.Mine(c.Request.Context(), UserID(c))
	if err != nil {
		failRoom(c, err)
		return
	}
	for i := range rooms {
		if rooms[i].Members == nil {
			rooms[i].Members = []model.MemberInfo{}
		}
	}
	OK(c, http.StatusOK, rooms)
}

// failRoom maps room and join errors to responses (design 5.8). Messages never
// carry the submitted code.
func failRoom(c *gin.Context, err error) {
	var already *service.AlreadyMemberError
	var cooldown *service.CooldownError
	var limited *service.RateLimitedError
	var issue *service.CodeIssueLimitError
	switch {
	case errors.Is(err, service.ErrRoomNotFound):
		Fail(c, http.StatusNotFound, model.CodeRoomNotFound, "room not found")
	case errors.Is(err, service.ErrNotRoomOwner):
		Fail(c, http.StatusForbidden, model.CodeNotRoomOwner, "not the room owner")
	case errors.Is(err, service.ErrOwnerCannotLeave):
		Fail(c, http.StatusConflict, model.CodeOwnerCannotLeave, "the owner cannot leave; dissolve the room instead")
	case errors.Is(err, service.ErrCannotRemoveOwner):
		Fail(c, http.StatusBadRequest, model.CodeValidationFailed, "cannot remove the room owner")
	case errors.As(err, &issue):
		FailDetails(c, http.StatusConflict, model.CodeCodeIssueLimit, "cannot issue another code", gin.H{"reason": issue.Reason})
	case errors.Is(err, service.ErrCodeInvalid):
		Fail(c, http.StatusNotFound, model.CodeCodeInvalid, "invite code invalid")
	case errors.Is(err, service.ErrRoomFull):
		Fail(c, http.StatusConflict, model.CodeRoomFull, "room full")
	case errors.As(err, &already):
		FailDetails(c, http.StatusConflict, model.CodeAlreadyMember, "already a member", gin.H{"room_id": already.RoomID})
	case errors.Is(err, service.ErrRoomLimit):
		Fail(c, http.StatusConflict, model.CodeRoomLimit, "room limit reached")
	case errors.Is(err, service.ErrAgeRestricted):
		Fail(c, http.StatusForbidden, model.CodeAgeRestricted, "age restricted")
	case errors.Is(err, service.ErrOnboardingRequired):
		Fail(c, http.StatusForbidden, model.CodeOnboardingRequired, "onboarding required")
	case errors.As(err, &cooldown):
		FailDetails(c, http.StatusTooManyRequests, model.CodeCodeCooldown, "too many wrong codes",
			gin.H{"retry_after_seconds": RetrySeconds(cooldown.RetryAfter)})
	case errors.As(err, &limited):
		c.Header("Retry-After", strconv.Itoa(RetrySeconds(limited.RetryAfter)))
		Fail(c, http.StatusTooManyRequests, model.CodeRateLimited, "too many requests")
	case errors.Is(err, service.ErrAccountDisabled):
		Fail(c, http.StatusForbidden, model.CodeAccountDisabled, "account disabled")
	case errors.Is(err, service.ErrUserNotFound):
		Fail(c, http.StatusUnauthorized, model.CodeUnauthorized, "unauthorized")
	default:
		_ = c.Error(err)
		Fail(c, http.StatusInternalServerError, model.CodeInternal, "internal error")
	}
}
