// Package service holds the business logic. It never writes SQL (that is the
// repository layer) and never depends on Gin.
package service

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrAccountDisabled = errors.New("account disabled")
	ErrRefreshInvalid  = errors.New("refresh token invalid")
	ErrRefreshReused   = errors.New("refresh token reused")
	ErrUserNotFound    = errors.New("user not found")

	ErrOnboardingRequired = errors.New("onboarding required")
	ErrCodeInvalid        = errors.New("invite code invalid")
	ErrRoomFull           = errors.New("room full")
	ErrRoomLimit          = errors.New("room limit reached")
	ErrAgeRestricted      = errors.New("age restricted")
	ErrRoomNotFound       = errors.New("room not found")

	ErrNotRoomOwner      = errors.New("not the room owner")
	ErrOwnerCannotLeave  = errors.New("owner cannot leave own room")
	ErrCannotRemoveOwner = errors.New("cannot remove the room owner")

	ErrDeliveryNotFound = errors.New("delivery not found")
	ErrPolishNotReady   = errors.New("polish not ready")

	ErrAIUnavailable    = errors.New("ai: every provider failed")
	ErrAIBudgetExceeded = errors.New("ai: daily hard cap reached")
)

// Reasons of a CodeIssueLimitError, in the order the checks run.
const (
	ReasonRoomFull      = "room_full"
	ReasonEnoughUnused  = "enough_unused"
	ReasonDailyLimit    = "daily_limit"
	ReasonLifetimeLimit = "lifetime_limit"
)

// CodeIssueLimitError reports why the owner cannot issue another code.
type CodeIssueLimitError struct{ Reason string }

func (e *CodeIssueLimitError) Error() string { return "code issue limit: " + e.Reason }

// AlreadyMemberError reports that the user already belongs to RoomID.
type AlreadyMemberError struct{ RoomID string }

func (e *AlreadyMemberError) Error() string { return "already a member of room " + e.RoomID }

// CooldownError reports a join cooldown that ends after RetryAfter.
type CooldownError struct{ RetryAfter time.Duration }

func (e *CooldownError) Error() string { return "join cooldown active" }

// RateLimitedError reports a rate limit that lifts after RetryAfter.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string { return "rate limited" }

// ValidationError lists the names of the invalid request fields.
type ValidationError struct{ Fields []string }

func (e *ValidationError) Error() string {
	return "invalid fields: " + strings.Join(e.Fields, ",")
}
