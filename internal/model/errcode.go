// Package model holds entities, DTOs and constants shared by every layer.
// It must not depend on any other internal package.
package model

import "errors"

// Business error codes returned in {"error":{"code":...}} (backend TRD chapter 8).
const (
	CodeInternal = "INTERNAL"
	CodeNotReady = "NOT_READY"

	CodeUnauthorized          = "UNAUTHORIZED"
	CodeValidationFailed      = "VALIDATION_FAILED"
	CodeAccountDisabled       = "ACCOUNT_DISABLED"
	CodeRefreshReused         = "REFRESH_REUSED"
	CodeRefreshInvalid        = "REFRESH_INVALID"
	CodeProviderNotConfigured = "PROVIDER_NOT_CONFIGURED"

	CodeOnboardingRequired = "ONBOARDING_REQUIRED"
	CodeCodeInvalid        = "CODE_INVALID"
	CodeRoomFull           = "ROOM_FULL"
	CodeAlreadyMember      = "ALREADY_MEMBER"
	CodeRoomLimit          = "ROOM_LIMIT"
	CodeAgeRestricted      = "AGE_RESTRICTED"
	CodeCodeCooldown       = "CODE_COOLDOWN"
	CodeRateLimited        = "RATE_LIMITED"
	CodeRoomNotFound       = "ROOM_NOT_FOUND"
	CodeNotRoomOwner       = "NOT_ROOM_OWNER"
	CodeOwnerCannotLeave   = "OWNER_CANNOT_LEAVE"
	CodeCodeIssueLimit     = "CODE_ISSUE_LIMIT"

	CodeDeliveryNotFound = "DELIVERY_NOT_FOUND"
	CodePolishNotReady   = "POLISH_NOT_READY"
	CodeQuotaExceeded    = "QUOTA_EXCEEDED"
)

// ErrNotFound is returned by repositories when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrAlreadyExists is returned by repositories on a unique-constraint conflict.
var ErrAlreadyExists = errors.New("already exists")

// Keys under which middleware stores request-scoped values in gin.Context.
// Middleware writes them and handlers read them, so neither depends on the other.
const (
	CtxRequestID = "request_id"
	CtxUserID    = "auth_user_id"
)
