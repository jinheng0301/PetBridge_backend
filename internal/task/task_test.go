package task

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
)

func TestTranslateHandler(t *testing.T) {
	var got string
	h := NewTranslateHandler(func(_ context.Context, id string) error { got = id; return nil })
	if err := h.ProcessTask(context.Background(), asynq.NewTask("translate", []byte(`{"message_id":"m1"}`))); err != nil {
		t.Fatal(err)
	}
	if got != "m1" {
		t.Fatalf("process got %q", got)
	}

	err := h.ProcessTask(context.Background(), asynq.NewTask("translate", []byte(`{bad`)))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("bad payload err = %v, want SkipRetry", err)
	}
	err = h.ProcessTask(context.Background(), asynq.NewTask("translate", []byte(`{}`)))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("empty id err = %v, want SkipRetry", err)
	}
}

func TestTranslateHandler_ProcessErrorRetries(t *testing.T) {
	boom := errors.New("boom")
	h := NewTranslateHandler(func(context.Context, string) error { return boom })
	err := h.ProcessTask(context.Background(), asynq.NewTask("translate", []byte(`{"message_id":"m1"}`)))
	if !errors.Is(err, boom) || errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunSweeper_Ticks(t *testing.T) {
	var n atomic.Int32
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	log.Warn("warm up") // the first slog call can take ~100ms on Windows and would eat the timing window
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunSweeper(ctx, 10*time.Millisecond, func(context.Context) (int, error) {
			n.Add(1)
			return 0, errors.New("sweep failed") // must not stop the loop
		}, log)
		close(done)
	}()
	time.Sleep(55 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunSweeper did not return after cancel")
	}
	if got := n.Load(); got < 4 {
		t.Fatalf("sweep called %d times, want >= 4", got)
	}
}
