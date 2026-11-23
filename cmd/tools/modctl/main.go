// Command modctl is the operator CLI for moderation: review flags and
// reports, ban and unban users.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jinheng0301/petbridge/internal/config"
	"github.com/jinheng0301/petbridge/internal/platform/db"
)

func main() {
	cfg, err := config.LoadFromProcess()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "modctl:", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := Run(ctx, pool, os.Args[1:], os.Stdout, time.Now); err != nil {
		fmt.Fprintln(os.Stderr, "modctl:", err)
		os.Exit(1)
	}
}
