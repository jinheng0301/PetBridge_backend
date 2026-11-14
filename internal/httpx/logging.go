// Package httpx holds HTTP middleware, the response envelope and logging setup.
package httpx

import (
	"io"
	"log/slog"
)

// NewLogger returns a JSON slog logger at the given level (debug|info|warn|error).
func NewLogger(level string, w io.Writer) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv}))
}
