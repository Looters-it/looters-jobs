package config

import (
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "postgresql://x", "KAFKA_BROKERS": "k1:9092, k2:9092"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != time.Second || cfg.Workers != 4 || cfg.MaxAttempts != 5 || cfg.StaleAfter != 5*time.Minute {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.KafkaBrokers) != 2 || cfg.KafkaBrokers[1] != "k2:9092" || cfg.KafkaBattleTopic != "battle.resolve" || cfg.KafkaGroup != "looters-jobs" {
		t.Fatalf("unexpected kafka defaults: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": "postgresql://x", "KAFKA_BROKERS": "k:9092", "JOBS_POLL_MS": "250", "JOBS_WORKERS": "8"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != 250*time.Millisecond || cfg.Workers != 8 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(env(nil)); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("expected DATABASE_URL error, got %v", err)
	}
	if _, err := Load(env(map[string]string{"DATABASE_URL": "postgresql://x", "KAFKA_BROKERS": " , "})); err == nil || !strings.Contains(err.Error(), "KAFKA_BROKERS") {
		t.Fatalf("expected KAFKA_BROKERS error, got %v", err)
	}
	for _, bad := range []string{"0", "-1", "abc"} {
		_, err := Load(env(map[string]string{"DATABASE_URL": "postgresql://x", "KAFKA_BROKERS": "k:9092", "JOBS_WORKERS": bad}))
		if err == nil || !strings.Contains(err.Error(), "JOBS_WORKERS") {
			t.Fatalf("JOBS_WORKERS=%q: expected error, got %v", bad, err)
		}
	}
}
