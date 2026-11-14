// Command seed writes local test data: six onboarded users and the zero invite
// code DEVA2345. It refuses to run unless APP_ENV=local.
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
	if cfg.AppEnv != "local" {
		fmt.Fprintln(os.Stderr, "seed: refusing to run unless APP_ENV=local")
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := Run(ctx, pool, time.Now); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
	for _, s := range seedUsers {
		fmt.Printf("%-12s %s %s\n", s.email, s.in.NativeLang, s.in.MBTI)
	}
	fmt.Println("zero invite code:", zeroCode)
}
