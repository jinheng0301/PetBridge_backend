package redis

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func testURL() string {
	if v := os.Getenv("TEST_REDIS_URL"); v != "" {
		return v
	}
	return "redis://127.0.0.1:6379/0"
}

func TestOpen_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("needs redis")
	}
	ctx := context.Background()
	c, err := Open(ctx, testURL())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := Ping(ctx, c); err != nil {
		t.Fatal(err)
	}
}

func TestOpen_BadURL_ErrorHidesPassword(t *testing.T) {
	_, err := Open(context.Background(), "redis://:hunter2@127.0.0.1:1/0")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks password: %v", err)
	}
	_, err = Open(context.Background(), "http://:hunter2@host")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("parse error must exist and hide password: %v", err)
	}
}

// A server that accepts connections but never answers must not stall Ping
// beyond its own 2 second limit (go-redis alone would wait 3 seconds).
func TestPing_TimesOutOnSilentServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // held open, never answered
		}
	}()
	c, err := New("redis://" + ln.Addr().String() + "/0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	if err := Ping(context.Background(), c); err == nil {
		t.Fatal("expected timeout error")
	}
	if d := time.Since(start); d < time.Second || d > 2500*time.Millisecond {
		t.Fatalf("Ping returned after %v, want about 2s", d)
	}
}

func TestOpen_KeepsCauseOfConnectionFailure(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close() // nothing listens here any more
	_, err := Open(context.Background(), "redis://:hunter2@"+addr+"/0")
	if err == nil || errors.Unwrap(err) == nil {
		t.Fatalf("want wrapped cause, got %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks password: %v", err)
	}
}

func TestNew_DoesNotConnect(t *testing.T) {
	c, err := New("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := New("http://:hunter2@host"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("bad scheme must fail without leaking: %v", err)
	}
}
