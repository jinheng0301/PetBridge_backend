// Package service holds the business logic. It never writes SQL (that is the
// repository layer) and never depends on Gin.
package service

import (
	"errors"
	"strings"
)

var (
	ErrAccountDisabled = errors.New("account disabled")
	ErrRefreshInvalid  = errors.New("refresh token invalid")
	ErrRefreshReused   = errors.New("refresh token reused")
	ErrUserNotFound    = errors.New("user not found")
)

// ValidationError lists the names of the invalid request fields.
type ValidationError struct{ Fields []string }

func (e *ValidationError) Error() string {
	return "invalid fields: " + strings.Join(e.Fields, ",")
}
