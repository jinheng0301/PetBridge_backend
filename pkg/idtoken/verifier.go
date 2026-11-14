// Package idtoken verifies Apple / Google ID tokens.
package idtoken

import (
	"context"
	"errors"
)

// ErrProviderNotConfigured means real Apple/Google verification is not wired up yet.
var ErrProviderNotConfigured = errors.New("login provider not configured")

// Verifier validates a provider ID token and returns the stable subject.
type Verifier interface {
	Verify(ctx context.Context, idToken string) (subject string, err error)
}

// NotConfiguredVerifier is the B2 placeholder until developer accounts exist.
type NotConfiguredVerifier struct{}

func (NotConfiguredVerifier) Verify(context.Context, string) (string, error) {
	return "", ErrProviderNotConfigured
}
