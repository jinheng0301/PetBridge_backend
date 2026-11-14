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
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jinheng0301/petbridge/internal/config"
	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/middleware"
	"github.com/jinheng0301/petbridge/internal/platform/db"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
	pbredis "github.com/jinheng0301/petbridge/internal/platform/redis"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/server"
	"github.com/jinheng0301/petbridge/internal/service"
	"github.com/jinheng0301/petbridge/pkg/idtoken"
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

	log := logger.New(cfg.LogLevel, os.Stdout)
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

	health := handler.NewHealthHandler(
		func(c context.Context) error { return db.Ping(c, pool) },
		func(c context.Context) error {
			if rdb == nil {
				return fmt.Errorf("redis not configured")
			}
			return pbredis.Ping(c, rdb)
		},
	)

	// repository -> service -> handler
	tx := repository.NewTxManager(pool)
	users := repository.NewUserRepo(pool)
	issuer := service.NewIssuer(cfg.JWTSigningKey, cfg.AccessTokenTTL, time.Now)
	authSvc := service.NewAuthService(users, repository.NewAuthIdentityRepo(pool),
		repository.NewRefreshTokenRepo(pool), tx, issuer, cfg.RefreshTokenTTL)
	userSvc := service.NewUserService(users, repository.NewRoomRepo(pool), repository.NewRoomMemberRepo(pool),
		repository.NewInviteCodeRepo(pool), tx, cfg.CodeTTLDays, time.Now)

	authHandler := handler.NewAuthHandler(authSvc, cfg.DevLoginEnabled, idtoken.NotConfiguredVerifier{}, idtoken.NotConfiguredVerifier{})
	userHandler := handler.NewUserHandler(userSvc, middleware.Auth(issuer))
	router := server.NewRouter(log, health, authHandler, userHandler)

	log.Info("starting api", "app_env", cfg.AppEnv, "port", cfg.AppPort)
	if err := server.Run(ctx, ":"+strconv.Itoa(cfg.AppPort), router, log); err != nil {
		log.Error("server stopped", "error", err.Error())
		os.Exit(1)
	}
}
