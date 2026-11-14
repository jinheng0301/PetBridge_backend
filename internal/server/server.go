package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const shutdownTimeout = 10 * time.Second

// Run serves until ctx is cancelled, then shuts down gracefully (in-flight
// requests get up to 10 seconds). It returns an error if the address cannot
// be bound (for example, port already in use).
func Run(ctx context.Context, addr string, handler http.Handler, l *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	l.Info("http server listening", "addr", addr)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	l.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return err
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
