// Command jobs runs the background work of the game: it consumes the queue
// table written by the api (geo processing, match start, turn close) and the
// Kafka topic of battle resolution requests.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"gioco/jobs/internal/battle"
	"gioco/jobs/internal/battlequeue"
	"gioco/jobs/internal/config"
	"gioco/jobs/internal/db"
	"gioco/jobs/internal/geoprocess"
	"gioco/jobs/internal/handlers"
	"gioco/jobs/internal/matchstart"
	"gioco/jobs/internal/queue"
	"gioco/jobs/internal/turnclose"
)

func main() {
	// LOG_LEVEL: debug, info (the default), warn or error.
	var level slog.Level
	if err := level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	if err := run(log); err != nil {
		log.Error("jobs stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	log.Info("waiting for the api to apply migrations")
	if err := db.WaitForSchema(ctx, pool); err != nil {
		return err
	}

	battles := battle.NewService(pool, log)
	worker := &queue.Worker{
		Store: queue.PgStore{Pool: pool},
		Handlers: handlers.All(pool,
			&geoprocess.Processor{Pool: pool, UploadDir: cfg.UploadDir, Geo: cfg.Geo, Log: log},
			&matchstart.Starter{Pool: pool, Log: log},
			&turnclose.Closer{Pool: pool, Log: log, Battles: battles},
		),
		Workers:      cfg.Workers,
		MaxAttempts:  cfg.MaxAttempts,
		PollInterval: cfg.PollInterval,
		StaleAfter:   cfg.StaleAfter,
		Log:          log,
	}
	battleConsumer, err := battlequeue.New(battlequeue.Options{
		Brokers: cfg.KafkaBrokers,
		Topic:   cfg.KafkaBattleTopic,
		Group:   cfg.KafkaGroup,
		Resolve: handlers.ResolveBattle(battles),
		Log:     log,
	})
	if err != nil {
		return err
	}

	log.Info("jobs started", "workers", cfg.Workers, "pollInterval", cfg.PollInterval.String(), "battleTopic", cfg.KafkaBattleTopic)
	var wg sync.WaitGroup
	wg.Go(func() { worker.Run(ctx) })
	wg.Go(func() { battleConsumer.Run(ctx) })
	wg.Wait()
	log.Info("jobs stopped")
	return nil
}
