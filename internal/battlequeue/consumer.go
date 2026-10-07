// Package battlequeue consumes the Kafka topic on which the api asks for a
// battle to be resolved. Battles are resolved only here, so the formula
// exists in one place.
package battlequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

// Message asks for one battle to be resolved. It carries ids only: the
// battle and the defender's recorded response are read from the database.
type Message struct {
	BattleID string `json:"battleId"`
	MatchID  string `json:"matchId"`
}

// Resolver resolves a battle. Delivery is at-least-once, so it must be
// idempotent: resolving a battle that is no longer open is a no-op.
type Resolver func(ctx context.Context, message Message) error

// Decode parses and validates a record value.
func Decode(value []byte) (Message, error) {
	var message Message
	if err := json.Unmarshal(value, &message); err != nil {
		return Message{}, fmt.Errorf("invalid battle message: %w", err)
	}
	if message.BattleID == "" || message.MatchID == "" {
		return Message{}, errors.New("invalid battle message: battleId and matchId are required")
	}
	return message, nil
}

// Options configures a Consumer.
type Options struct {
	Brokers []string
	Topic   string
	Group   string
	Resolve Resolver
	Log     *slog.Logger
	// MaxAttempts per message before it is skipped; a skipped battle is
	// still resolved when its turn closes. Defaults to 5.
	MaxAttempts int
	// RetryDelay between attempts on the same message. Defaults to 2s.
	RetryDelay time.Duration
}

// Consumer reads the battle topic as part of a consumer group.
type Consumer struct {
	client *kgo.Client
	opts   Options
}

// New creates the Kafka client. It does not block on the broker being
// reachable: the client keeps retrying while Run is polling.
func New(opts Options) (*Consumer, error) {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = 2 * time.Second
	}
	client, err := kgo.NewClient(
		kgo.SeedBrokers(opts.Brokers...),
		kgo.ConsumerGroup(opts.Group),
		kgo.ConsumeTopics(opts.Topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// Offsets are committed only after the battles have been resolved.
		kgo.DisableAutoCommit(),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return &Consumer{client: client, opts: opts}, nil
}

// Run consumes until ctx is cancelled, then leaves the group.
// A rebalance while a batch is being resolved can deliver a battle twice,
// which the idempotent resolver absorbs.
func (c *Consumer) Run(ctx context.Context) {
	defer c.client.Close()
	for {
		fetches := c.client.PollFetches(ctx)
		if ctx.Err() != nil {
			return
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			c.opts.Log.Error("kafka fetch error", "topic", topic, "partition", partition, "err", err)
		})

		// Partitions run in parallel; within a partition (one match is always
		// on the same partition) battles are resolved in order.
		var wg sync.WaitGroup
		fetches.EachPartition(func(partition kgo.FetchTopicPartition) {
			wg.Go(func() {
				for _, record := range partition.Records {
					c.handle(ctx, record)
				}
			})
		})
		wg.Wait()

		if ctx.Err() == nil {
			if err := c.client.CommitUncommittedOffsets(ctx); err != nil {
				c.opts.Log.Error("kafka commit failed", "err", err)
			}
		}
	}
}

// handle resolves one record, retrying a few times before giving up on it.
func (c *Consumer) handle(ctx context.Context, record *kgo.Record) {
	message, err := Decode(record.Value)
	if err != nil {
		c.opts.Log.Error("skipping battle message", "partition", record.Partition, "offset", record.Offset, "err", err)
		return
	}
	for attempt := 1; ; attempt++ {
		err := c.opts.Resolve(ctx, message)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if attempt >= c.opts.MaxAttempts {
			c.opts.Log.Error("battle left to the turn close", "battleId", message.BattleID, "attempts", attempt, "err", err)
			return
		}
		c.opts.Log.Warn("battle resolution failed, retrying", "battleId", message.BattleID, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.opts.RetryDelay):
		}
	}
}
