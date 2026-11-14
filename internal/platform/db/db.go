// Package db wraps the PostgreSQL connection pool and migrations.
package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const pingTimeout = 2 * time.Second

// Open builds a pool and pings it. Errors never include the DSN or password.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("postgres: invalid DATABASE_URL")
	}
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("postgres: cannot create pool")
	}
	if err := Ping(ctx, pool); err != nil {
		pool.Close()
		return nil, errors.New("postgres: connection failed (" + cfg.ConnConfig.Host + ")")
	}
	return pool, nil
}

// Ping checks the pool with a 2 second timeout.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return pool.Ping(ctx)
}
