package service

import "context"

// RetryTx runs a transaction at most twice: a second attempt happens only when
// the first failed with an error Retryable accepts (deadlock, serialization
// failure) and ctx is still live. fn must be safe to run again; every service
// transaction here is, because a failed attempt rolled back completely and a
// committed join is answered as a replay.
type RetryTx struct {
	Inner     TxRunner
	Retryable func(error) bool
}

func (r RetryTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	err := r.Inner.WithTx(ctx, fn)
	if err != nil && r.Retryable(err) && ctx.Err() == nil {
		return r.Inner.WithTx(ctx, fn)
	}
	return err
}
