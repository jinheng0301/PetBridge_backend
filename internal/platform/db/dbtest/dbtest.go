// Package dbtest provides a clean, fully migrated database for integration tests.
package dbtest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jinheng0301/petbridge/internal/platform/db"
)

const defaultURL = "postgres://petbridge:petbridge@localhost:5432/petbridge_test?sslmode=disable"

// URL returns the test database URL.
func URL() string {
	if v := os.Getenv("TEST_DATABASE_URL"); v != "" {
		return v
	}
	return defaultURL
}

// Reset wipes the public schema of the test database, applies all migrations
// and returns a pool. Skipped under -short.
func Reset(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("needs postgres")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, URL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateUp(ctx, URL()); err != nil {
		t.Fatal(err)
	}
	return pool
}
