// Package dbtest provides a clean, fully migrated database for integration tests.
package dbtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
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

// ensureTestDB refuses any database whose name does not end in "_test":
// Reset drops the whole public schema, so a DATABASE_URL pasted into
// TEST_DATABASE_URL must never get that far.
func ensureTestDB(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("dbtest: unparsable test database URL")
	}
	if name := strings.TrimPrefix(u.Path, "/"); !strings.HasSuffix(name, "_test") {
		return fmt.Errorf("dbtest: refusing to reset database %q: name must end in _test", name)
	}
	return nil
}

// Reset wipes the public schema of the test database, applies all migrations
// and returns a pool. Skipped under -short.
func Reset(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("needs postgres")
	}
	if err := ensureTestDB(URL()); err != nil {
		t.Fatal(err)
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
