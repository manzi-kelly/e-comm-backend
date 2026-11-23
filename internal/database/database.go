// Package database owns PostgreSQL pooling, timeout-aware SQL helpers,
// transactions and the forward-only migration runner.
package database

import (
	"context"
	"fmt"
	"time"

	"autivashop/backend/internal/config"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is the application's database handle.
type DB struct {
	pool         *pgxpool.Pool
	queryTimeout time.Duration
}

// New creates a pool and verifies connectivity. Returned errors are generic
// so connection strings and credentials are never exposed to callers or logs.
func New(ctx context.Context, cfg config.DatabaseConfig) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid database configuration")
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.HealthCheckPeriod = cfg.HealthCheckPeriod
	poolCfg.ConnConfig.ConnectTimeout = cfg.QueryTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database pool initialization failed")
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.QueryTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database connection failed")
	}
	return &DB{pool: pool, queryTimeout: cfg.QueryTimeout}, nil
}

// Ping checks database connectivity with the configured query timeout.
func (d *DB) Ping(ctx context.Context) error {
	pingCtx, cancel := d.withTimeout(ctx)
	defer cancel()
	return d.pool.Ping(pingCtx)
}

// Close releases all pooled connections.
func (d *DB) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}

// Pool exposes the pool for advanced database access. Callers must apply
// timeouts themselves; prefer the helpers on DB for ordinary queries.
func (d *DB) Pool() *pgxpool.Pool {
	return d.pool
}

// Exec runs a statement with the configured query timeout.
func (d *DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	queryCtx, cancel := d.withTimeout(ctx)
	defer cancel()
	return d.pool.Exec(queryCtx, sql, args...)
}

// WithTx runs fn inside a transaction and rolls back on any error.
func (d *DB) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	txCtx, cancel := d.withTimeout(ctx)
	defer cancel()

	tx, err := d.pool.Begin(txCtx)
	if err != nil {
		return fmt.Errorf("could not begin transaction")
	}
	committed := false
	defer func() {
		if !committed {
			rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer rollbackCancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(txCtx); err != nil {
		return fmt.Errorf("could not commit transaction")
	}
	committed = true
	return nil
}

func (d *DB) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if d == nil || d.queryTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d.queryTimeout)
}
