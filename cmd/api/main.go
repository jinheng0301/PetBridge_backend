// Command api is the PetBridge HTTP API process.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jinheng0301/petbridge/internal/config"
	"github.com/jinheng0301/petbridge/internal/handler"
	"github.com/jinheng0301/petbridge/internal/middleware"
	"github.com/jinheng0301/petbridge/internal/platform/db"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
	"github.com/jinheng0301/petbridge/internal/platform/ratelimit"
	"github.com/jinheng0301/petbridge/internal/platform/realtime"
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
	// A malformed URL is a config error and stops startup; only a failed
	// connection degrades. The client is lazy, so it reconnects once Redis is back.
	rdb, err := pbredis.New(cfg.RedisURL)
	if err != nil {
		log.Error("invalid redis configuration", "error", err.Error())
		os.Exit(1)
	}
	defer rdb.Close()
	if err := pbredis.Ping(ctx, rdb); err != nil {
		log.Warn("redis unavailable, starting degraded", "error", err.Error())
	}

	health := handler.NewHealthHandler(
		func(c context.Context) error { return db.Ping(c, pool) },
		func(c context.Context) error { return pbredis.Ping(c, rdb) },
	)

	// repository -> service -> handler
	tx := repository.NewTxManager(pool)
	users := repository.NewUserRepo(pool)
	rooms := repository.NewRoomRepo(pool)
	members := repository.NewRoomMemberRepo(pool)
	codes := repository.NewInviteCodeRepo(pool)
	issuer := service.NewIssuer(cfg.JWTSigningKey, cfg.AccessTokenTTL, time.Now)
	authSvc := service.NewAuthService(users, repository.NewAuthIdentityRepo(pool),
		repository.NewRefreshTokenRepo(pool), tx, issuer, cfg.RefreshTokenTTL)
	userSvc := service.NewUserService(users, rooms, members, codes, tx, cfg.CodeTTLDays, time.Now).
		WithRoomCapacity(cfg.RoomCapacity)

	// Rate limits live in Redis and fall back to in-process counters while it is down.
	counter := ratelimit.NewFallback(ratelimit.NewRedis(rdb), ratelimit.NewMemory(time.Now),
		newFallbackLogger(log, "redis unavailable, rate limits use in-process counters", 30*time.Second, time.Now))

	// Realtime: room seqs and ws:events in Redis; while Redis is down, events
	// are delivered by this node's hub directly (backend TRD 7.3).
	rtCfg := service.DefaultRealtimeConfig(cfg.MinAppVersion)
	hub := service.NewHub(rtCfg)
	pub := service.NewEventPublisher(realtime.NewBus(rdb, "ws:events", ""), hub,
		newFallbackLogger(log, "redis unavailable, realtime events delivered locally", 30*time.Second, time.Now))
	presence := service.NewPresenceService(realtime.NewPresenceStore(rdb, "", rtCfg.PresenceTTL), hub, rooms, pub, rtCfg, time.Now)
	pub.SetPresence(presence)
	pub.SetLogger(log)
	go pub.Run(ctx)
	// A deadlock or serialization failure rolls the transaction back whole; run it once more.
	retryTx := service.RetryTx{Inner: tx, Retryable: repository.IsRetryableTxError}
	bans := repository.NewBanRepo(pool)
	joinSvc := service.NewJoinService(users, rooms, members, codes, bans,
		repository.NewRewardRepo(pool), retryTx, ratelimit.NewJoinGuard(counter), pub,
		service.JoinConfig{
			CodeTTLDays:       cfg.CodeTTLDays,
			RewardCount:       cfg.RewardCodeCount,
			RewardLifetimeCap: cfg.RewardCodeLifetimeCap,
		}, time.Now)

	authn := middleware.Auth(issuer, userSvc)
	var googleVerifier idtoken.Verifier = idtoken.NotConfiguredVerifier{}
	if cfg.GoogleClientID != "" {
		googleVerifier = idtoken.NewGoogleVerifier(cfg.GoogleClientID)
	}
	authHandler := handler.NewAuthHandler(authSvc, cfg.DevLoginEnabled, idtoken.NotConfiguredVerifier{}, googleVerifier).
		WithLoginLimit(middleware.RateLimitIP(counter, "login", 10, time.Minute))
	userHandler := handler.NewUserHandler(userSvc, authn)
	manageSvc := service.NewManageService(users, rooms, members, codes, bans, retryTx, pub,
		service.ManageConfig{
			CodeTTLDays:     cfg.CodeTTLDays,
			RegenDailyLimit: cfg.OwnerRegenDailyLimit,
			LifetimeCap:     cfg.RewardCodeLifetimeCap,
		}, time.Now)
	roomHandler := handler.NewRoomHandler(joinSvc, service.NewRoomService(rooms), authn).WithManage(manageSvc)
	codeHandler := handler.NewCodeHandler(service.NewCodeService(codes, time.Now), authn)
	// Upgrades are limited per IP (generous: carrier NAT shares IPs) and per
	// user; the socket itself closes when the access token expires.
	wsHandler := handler.NewWsHandler(hub, presence, service.NewRealtimeService(rooms, members, presence, pub, pub),
		users, issuer, rtCfg, authn).
		WithUpgradeLimit(middleware.RateLimitIP(counter, "ws", 60, time.Minute),
			middleware.RateLimitUser(counter, "ws", 20, time.Minute)).
		WithLogger(log)
	router := server.NewRouter(log, health, authHandler, userHandler, roomHandler, codeHandler, wsHandler)

	log.Info("starting api", "app_env", cfg.AppEnv, "port", cfg.AppPort)
	if err := server.Run(ctx, net.JoinHostPort(cfg.AppHost, strconv.Itoa(cfg.AppPort)), router, log, func() { wsHandler.Shutdown(5 * time.Second) }); err != nil {
		log.Error("server stopped", "error", err.Error())
		os.Exit(1)
	}
}
