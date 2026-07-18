package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds the database connection configuration.
type Config struct {
	DSN      string
	MaxConns int32
	MinConns int32
}

// Pool wraps a pgxpool.Pool for use across the application.
type Pool struct {
	pool *pgxpool.Pool
}

// NewPool creates a new connection pool, connects, and pings the database.
func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 20
	}
	if cfg.MinConns == 0 {
		cfg.MinConns = 2
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db.NewPool: parse config: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db.NewPool: connect: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db.NewPool: ping: %w", err)
	}

	return &Pool{pool: pool}, nil
}

// Close closes all connections in the pool.
func (p *Pool) Close() {
	p.pool.Close()
}

// Raw returns the underlying pgxpool.Pool, for callers that need a dedicated connection outside
// the query interfaces this package exposes (e.g. shared/leaderelection's advisory lock, which
// must hold one connection for as long as it holds leadership).
func (p *Pool) Raw() *pgxpool.Pool {
	return p.pool
}
