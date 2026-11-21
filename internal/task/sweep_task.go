package task

import (
	"context"
	"log/slog"
	"time"
)

// RunSweeper calls sweep every `every` until ctx ends. Errors are logged only.
func RunSweeper(ctx context.Context, every time.Duration, sweep func(ctx context.Context) (int, error), log *slog.Logger) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			n, err := sweep(ctx)
			if err != nil {
				log.Warn("translate sweep failed", "error", err.Error())
				continue
			}
			if n > 0 {
				log.Info("translate sweep enqueued", "count", n)
			}
		}
	}
}
