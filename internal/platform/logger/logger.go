// Package logger builds the process-wide structured logger.
package logger

import (
	"io"
	"log/slog"
)

// New returns a JSON slog logger at the given level (debug|info|warn|error).
func New(level string, w io.Writer) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}
