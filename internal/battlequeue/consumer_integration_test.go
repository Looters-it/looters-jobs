package battlequeue_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"looters/jobs/internal/battlequeue"
)

// Needs a running broker: set TEST_KAFKA_BROKERS (see .env.example).
func TestConsumerResolvesEveryBattleInOrderPerMatch(t *testing.T) {
	raw := os.Getenv("TEST_KAFKA_BROKERS")
	if raw == "" {
		t.Skip("TEST_KAFKA_BROKERS not set")
	}
	brokers := strings.Split(raw, ",")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	topic := fmt.Sprintf("test.battle.resolve.%d", time.Now().UnixNano())
	producer, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer producer.Close()
	admin := kadm.NewClient(producer)
	if _, err := admin.CreateTopic(ctx, 3, 1, nil, topic); err != nil {
		t.Fatal(err)
	}
	defer admin.DeleteTopics(context.Background(), topic)

	const matches, perMatch = 4, 10
	for m := range matches {
		for b := range perMatch {
			match := fmt.Sprintf("m%d", m)
			value := fmt.Sprintf(`{"battleId":"%s-b%02d","matchId":"%s"}`, match, b, match)
			producer.Produce(ctx, &kgo.Record{Topic: topic, Key: []byte(match), Value: []byte(value)}, nil)
		}
	}
	// A malformed message must be skipped without blocking the partition.
	producer.Produce(ctx, &kgo.Record{Topic: topic, Key: []byte("m0"), Value: []byte("not json")}, nil)
	if err := producer.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	resolved := map[string][]string{}
	failedOnce := false
	total := 0
	done := make(chan struct{})
	consumer, err := battlequeue.New(battlequeue.Options{
		Brokers:    brokers,
		Topic:      topic,
		Group:      topic,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		RetryDelay: 10 * time.Millisecond,
		Resolve: func(_ context.Context, message battlequeue.Message) error {
			mu.Lock()
			defer mu.Unlock()
			// The first delivery fails once: the consumer must retry it in place.
			if !failedOnce {
				failedOnce = true
				return errors.New("transient")
			}
			resolved[message.MatchID] = append(resolved[message.MatchID], message.BattleID)
			if total++; total == matches*perMatch {
				close(done)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		consumer.Run(runCtx)
		close(finished)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("resolved %d of %d battles", total, matches*perMatch)
	}
	stop()
	<-finished

	mu.Lock()
	defer mu.Unlock()
	for m := range matches {
		match := fmt.Sprintf("m%d", m)
		got := resolved[match]
		if len(got) != perMatch {
			t.Fatalf("match %s: resolved %d battles, want %d", match, len(got), perMatch)
		}
		for b, id := range got {
			if want := fmt.Sprintf("%s-b%02d", match, b); id != want {
				t.Fatalf("match %s: position %d is %s, want %s (order broken)", match, b, id, want)
			}
		}
	}
}
