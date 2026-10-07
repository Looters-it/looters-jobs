// Package config reads the service configuration from the environment.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config holds everything the service reads from the environment.
type Config struct {
	DatabaseURL  string
	UploadDir    string
	PollInterval time.Duration
	Workers      int
	MaxAttempts  int
	StaleAfter   time.Duration

	// Kafka carries the battle resolution queue written by the api.
	KafkaBrokers     []string
	KafkaBattleTopic string
	KafkaGroup       string
}

// Load builds the configuration using getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		DatabaseURL: getenv("DATABASE_URL"),
		UploadDir:   getenv("UPLOAD_DIR"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.UploadDir == "" {
		cfg.UploadDir = "./uploads"
	}

	for broker := range strings.SplitSeq(getenv("KAFKA_BROKERS"), ",") {
		if broker = strings.TrimSpace(broker); broker != "" {
			cfg.KafkaBrokers = append(cfg.KafkaBrokers, broker)
		}
	}
	if len(cfg.KafkaBrokers) == 0 {
		return Config{}, fmt.Errorf("KAFKA_BROKERS is required")
	}
	cfg.KafkaBattleTopic = orDefault(getenv("KAFKA_BATTLE_TOPIC"), "battle.resolve")
	cfg.KafkaGroup = orDefault(getenv("KAFKA_GROUP_ID"), "gioco-jobs")

	pollMs, err := positiveInt(getenv, "JOBS_POLL_MS", 1000)
	if err != nil {
		return Config{}, err
	}
	staleSec, err := positiveInt(getenv, "JOBS_STALE_AFTER_SEC", 300)
	if err != nil {
		return Config{}, err
	}
	if cfg.Workers, err = positiveInt(getenv, "JOBS_WORKERS", 4); err != nil {
		return Config{}, err
	}
	if cfg.MaxAttempts, err = positiveInt(getenv, "JOBS_MAX_ATTEMPTS", 5); err != nil {
		return Config{}, err
	}
	cfg.PollInterval = time.Duration(pollMs) * time.Millisecond
	cfg.StaleAfter = time.Duration(staleSec) * time.Second
	return cfg, nil
}

func positiveInt(getenv func(string) string, name string, fallback int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}
	return value, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
