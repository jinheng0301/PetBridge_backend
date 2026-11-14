package redis

import (
	"context"
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

func TestPing_RespectsContextTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("needs redis")
	}
	c, err := Open(context.Background(), testURL())
	if err != nil {
		t.Fatal(err)
	}
	c.Close() // closed client: Ping must fail promptly
	start := time.Now()
	if err := Ping(context.Background(), c); err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Ping took %v", time.Since(start))
	}
}
