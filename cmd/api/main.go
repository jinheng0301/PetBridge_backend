// Command api is the PetBridge HTTP API process.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/redis/go-redis/v9"

	"github.com/jinheng0301/petbridge/internal/config"
	"github.com/jinheng0301/petbridge/internal/health"
	"github.com/jinheng0301/petbridge/internal/httpx"
	"github.com/jinheng0301/petbridge/internal/platform/db"
	pbredis "github.com/jinheng0301/petbridge/internal/platform/redis"
	"github.com/jinheng0301/petbridge/internal/server"
)

func main() {
	cfg, err := config.LoadFromProcess()
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			fmt.Fprintln(os.Stderr, "invalid configuration:")
			for _, p := range ve.Problems {
				fmt.Fprintln(os.Stderr, "  -", p)
			}
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}

	log := httpx.NewLogger(cfg.LogLevel, os.Stdout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database unavailable", "error", err.Error())
		os.Exit(1)
	}
	defer pool.Close()

	// Redis is optional at startup: the service degrades instead of failing (TRD 12.2).
	rdb, err := pbredis.Open(ctx, cfg.RedisURL)
	if err != nil {
		log.Warn("redis unavailable, starting degraded", "error", err.Error())
		// Lazy client: go-redis reconnects on its own once Redis is back.
		if opt, perr := redis.ParseURL(cfg.RedisURL); perr == nil {
			rdb = redis.NewClient(opt)
		}
	}
	if rdb != nil {
		defer rdb.Close()
	}

	h := health.New(
		func(c context.Context) error { return db.Ping(c, pool) },
		func(c context.Context) error {
			if rdb == nil {
				return fmt.Errorf("redis not configured")
			}
			return pbredis.Ping(c, rdb)
		},
	)
	router := server.NewRouter(log, h)

	log.Info("starting api", "app_env", cfg.AppEnv, "port", cfg.AppPort)
	if err := server.Run(ctx, ":"+strconv.Itoa(cfg.AppPort), router, log); err != nil {
		log.Error("server stopped", "error", err.Error())
		os.Exit(1)
	}
}
