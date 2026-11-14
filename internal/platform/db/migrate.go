package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/jinheng0301/petbridge/db/migrations"
)

func withGoose(databaseURL string, fn func(*sql.DB) error) error {
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errors.New("migrate: cannot open database")
	}
	defer sqlDB.Close()
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return fn(sqlDB)
}

// MigrateUp applies all pending migrations; safe to run repeatedly.
func MigrateUp(ctx context.Context, databaseURL string) error {
	return withGoose(databaseURL, func(d *sql.DB) error { return goose.UpContext(ctx, d, ".") })
}

// MigrateDown rolls back the most recent migration.
func MigrateDown(ctx context.Context, databaseURL string) error {
	return withGoose(databaseURL, func(d *sql.DB) error { return goose.DownContext(ctx, d, ".") })
}

// MigrateStatus reports the current schema version.
func MigrateStatus(ctx context.Context, databaseURL string) (string, error) {
	var out string
	err := withGoose(databaseURL, func(d *sql.DB) error {
		v, err := goose.GetDBVersionContext(ctx, d)
		out = fmt.Sprintf("current version: %d", v)
		return err
	})
	return out, err
}
