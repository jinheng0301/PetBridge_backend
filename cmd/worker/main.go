// Command worker runs the asynq translation and polish worker and the compensating sweep.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hibiken/asynq"

	"github.com/jinheng0301/petbridge/internal/bootstrap"
	"github.com/jinheng0301/petbridge/internal/config"
	"github.com/jinheng0301/petbridge/internal/platform/aistore"
	"github.com/jinheng0301/petbridge/internal/platform/db"
	"github.com/jinheng0301/petbridge/internal/platform/logger"
	"github.com/jinheng0301/petbridge/internal/platform/queue"
	"github.com/jinheng0301/petbridge/internal/platform/realtime"
	pbredis "github.com/jinheng0301/petbridge/internal/platform/redis"
	"github.com/jinheng0301/petbridge/internal/repository"
	"github.com/jinheng0301/petbridge/internal/service"
	"github.com/jinheng0301/petbridge/internal/task"
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

	rdb, err := pbredis.New(cfg.RedisURL)
	if err != nil {
		log.Error("invalid redis configuration", "error", err.Error())
		os.Exit(1)
	}
	defer rdb.Close()
	// The asynq server needs its own connections (blocking dequeues outlast
	// rdb's 2s read timeout); they are built from rdb's parsed options so the
	// worker and the api's queue always point at the same Redis and DB.
	redisOpt := queue.ServerRedisOpt(rdb)

	// The queue lives in Redis, so wait for it instead of exiting.
	if !waitForRedis(ctx, func(c context.Context) error { return pbredis.Ping(c, rdb) }, log) {
		return // interrupted while waiting
	}

	providers, err := bootstrap.Providers(ctx, cfg)
	if err != nil {
		log.Error("ai providers unavailable", "error", err.Error())
		os.Exit(1)
	}
	rtCfg := service.DefaultRealtimeConfig(cfg.MinAppVersion)
	// The worker never runs the hub, so it has no local connections; events go out through Redis.
	pub := service.NewEventPublisher(realtime.NewBus(rdb, "ws:events", ""), service.NewHub(rtCfg),
		newFallbackLogger(log, "redis unavailable, realtime events delivered locally", 30*time.Second, time.Now))
	pub.SetLogger(log)

	cache := aistore.NewCache(rdb)
	meter := aistore.NewCostMeter(rdb,
		newFallbackLogger(log, "redis unavailable, ai cost meter uses local counters", 30*time.Second, time.Now))
	gw := service.NewAIGateway(providers, cache, meter, service.GatewayConfig{
		TranslateTimeout:  3 * time.Second,
		PolishTimeout:     6 * time.Second,
		MaxConcurrent:     16,
		DailyBudgetMicros: cfg.AIDailyBudgetMicros,
		ZeroCost:          bootstrap.ZeroCost(cfg),
		Prices:            bootstrap.Prices(cfg),
	}, time.Now, log)

	store := repository.NewMessageRepo(pool)
	mod, err := service.LoadModerator()
	if err != nil {
		log.Error("moderation word lists unavailable", "error", err.Error())
		os.Exit(1)
	}
	members := repository.NewRoomMemberRepo(pool)
	usage := repository.NewAIUsageRepo(pool)
	q := queue.New(rdb) // shares rdb's fail-fast timeouts; rdb is closed above
	translation := service.NewTranslationService(store, members, gw, mod, pub, time.Now, log).
		WithUsage(usage).WithPolishQueue(q)
	polish := service.NewPolishService(store, members, gw, mod, usage, pub, time.Now, log)
	sweep := service.NewSweepService(store, q, time.Now, log).WithPolish(q, polish.GiveUp)

	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency:    cfg.WorkerConcurrency,
		Queues:         map[string]int{queue.QueueTranslate: 6, queue.QueuePolish: 3, "default": 1},
		RetryDelayFunc: queue.RetryDelay,
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc(queue.TypeTranslate, task.NewTranslateHandler(translation.Process))
	mux.HandleFunc(queue.TypePolish, task.NewPolishHandler(polish.Process))

	sweepCtx, cancelSweep := context.WithCancel(ctx)
	defer cancelSweep()
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		task.RunSweeper(sweepCtx, 60*time.Second, sweep.Sweep, log)
	}()

	if err := srv.Start(mux); err != nil {
		log.Error("worker failed to start", "error", err.Error())
		os.Exit(1)
	}
	log.Info("worker consuming", "app_env", cfg.AppEnv, "concurrency", cfg.WorkerConcurrency)
	<-ctx.Done()
	log.Info("worker shutting down")
	srv.Shutdown() // waits for running tasks
	cancelSweep()
	<-sweepDone // a sweep may be mid-run; finish before the deferred closes
}

// waitForRedis pings every 5s until Redis answers, warning every 30s. It
// returns false when ctx ends first.
func waitForRedis(ctx context.Context, ping func(context.Context) error, log interface {
	Warn(msg string, args ...any)
}) bool {
	err := ping(ctx)
	if err == nil {
		return true
	}
	lastWarn := time.Time{}
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()
	for {
		if lastWarn.IsZero() || time.Since(lastWarn) >= 30*time.Second {
			log.Warn("redis unavailable, worker waiting", "error", err.Error())
			lastWarn = time.Now()
		}
		select {
		case <-ctx.Done():
			return false
		case <-tk.C:
		}
		if err = ping(ctx); err == nil {
			return true
		}
	}
}
