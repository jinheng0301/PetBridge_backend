// Command migrate applies database migrations: migrate up|down|status.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jinheng0301/petbridge/internal/config"
	"github.com/jinheng0301/petbridge/internal/platform/db"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: migrate up|down|status")
		os.Exit(2)
	}
	cfg, err := config.LoadFromProcess()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	switch os.Args[1] {
	case "up":
		err = db.MigrateUp(ctx, cfg.DatabaseURL)
	case "down":
		err = db.MigrateDown(ctx, cfg.DatabaseURL)
	case "status":
		var s string
		if s, err = db.MigrateStatus(ctx, cfg.DatabaseURL); err == nil {
			fmt.Println(s)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: migrate up|down|status")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
	fmt.Println("ok")
}
