package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func testURL() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://petbridge:petbridge@localhost:5432/petbridge_test?sslmode=disable"
}

func TestOpen_Succeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("needs postgres")
	}
	ctx := context.Background()
	pool, err := Open(ctx, testURL())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Ping(ctx, pool); err != nil {
		t.Fatal(err)
	}
}

func TestOpen_BadURL_ErrorHidesPassword(t *testing.T) {
	_, err := Open(context.Background(), "postgres://u:hunter2@127.0.0.1:1/db")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks password: %v", err)
	}
	_, err = Open(context.Background(), "::not a url:hunter2")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("parse error must exist and hide password: %v", err)
	}
}

func TestPing_RespectsContextTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("needs postgres")
	}
	pool, err := Open(context.Background(), testURL())
	if err != nil {
		t.Fatal(err)
	}
	pool.Close() // closed pool: Ping must fail promptly, not hang
	start := time.Now()
	if err := Ping(context.Background(), pool); err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Ping took %v", time.Since(start))
	}
}
