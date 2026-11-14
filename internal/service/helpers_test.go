package service

import (
	"context"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

var bg = context.Background()

var codeRE = regexp.MustCompile(`^[2-9A-HJ-NP-Z]{8}$`)

func count(t *testing.T, p *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(bg, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
