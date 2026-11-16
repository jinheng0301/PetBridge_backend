package service

import (
	"context"
	"errors"
	"testing"
)

var errDeadlock = errors.New("deadlock")

// scriptTx runs fn for real, then substitutes the scripted outcomes for it.
type scriptTx struct {
	outcomes []error
	calls    int
	cancel   context.CancelFunc // cancelled after the first failing attempt, if set
}

func (s *scriptTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	i := s.calls
	s.calls++
	if err := fn(ctx); err != nil {
		return err
	}
	err := s.outcomes[min(i, len(s.outcomes)-1)]
	if err != nil && s.cancel != nil {
		s.cancel()
	}
	return err
}

func TestRetryTx(t *testing.T) {
	retryable := func(err error) bool { return errors.Is(err, errDeadlock) }
	other := errors.New("other")
	cases := []struct {
		name      string
		outcomes  []error
		cancelled bool
		wantCalls int
		wantErr   error
	}{
		{"retries once then succeeds", []error{errDeadlock, nil}, false, 2, nil},
		{"gives up after two attempts", []error{errDeadlock, errDeadlock}, false, 2, errDeadlock},
		{"does not retry other errors", []error{other}, false, 1, other},
		{"does not retry once ctx is done", []error{errDeadlock, nil}, true, 1, errDeadlock},
		{"success first time", []error{nil}, false, 1, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inner := &scriptTx{outcomes: c.outcomes}
			if c.cancelled {
				inner.cancel = cancel
			}
			runs := 0
			err := RetryTx{Inner: inner, Retryable: retryable}.WithTx(ctx, func(context.Context) error {
				runs++
				return nil
			})
			if !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) {
				t.Fatalf("err=%v want %v", err, c.wantErr)
			}
			if inner.calls != c.wantCalls || runs != c.wantCalls {
				t.Fatalf("inner calls=%d fn runs=%d, want %d", inner.calls, runs, c.wantCalls)
			}
		})
	}
}
