// Package db opens the connection pool shared with the api's database.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pool and waits until the database answers.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := retry(ctx, time.Second, pool.Ping); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database not reachable: %w", err)
	}
	return pool, nil
}

// WaitForSchema blocks until the api has applied its migrations. The schema
// belongs to the api: this service never creates or alters tables.
func WaitForSchema(ctx context.Context, pool *pgxpool.Pool) error {
	return retry(ctx, time.Second, func(ctx context.Context) error {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('"Job"') IS NOT NULL`).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf(`table "Job" not found: migrations not applied yet`)
		}
		return nil
	})
}

// retry calls fn until it succeeds or ctx ends, returning the last error.
func retry(ctx context.Context, every time.Duration, fn func(context.Context) error) error {
	for {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %v)", ctx.Err(), err)
		case <-time.After(every):
		}
	}
}
