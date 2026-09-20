package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ServiceMaxConns is the pool size every control-plane service runs with.
const ServiceMaxConns = 20

// Config holds the database connection configuration. MaxConns is required.
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
	if cfg.MaxConns <= 0 {
		return nil, fmt.Errorf("db.NewPool: MaxConns must be positive, got %d", cfg.MaxConns)
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

// AdvisoryLock is a session-level advisory lock held on one dedicated connection. Postgres
// releases it the moment that connection closes, including on a process crash.
type AdvisoryLock struct {
	conn *pgxpool.Conn
}

// TryAdvisoryLock takes the advisory lock key without waiting. It returns nil and no error when
// another session holds it.
func (p *Pool) TryAdvisoryLock(ctx context.Context, key int64) (*AdvisoryLock, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db.TryAdvisoryLock: acquire connection: %w", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); err != nil {
		conn.Release()
		return nil, fmt.Errorf("db.TryAdvisoryLock: %w", err)
	}
	if !ok {
		conn.Release()
		return nil, nil
	}
	return &AdvisoryLock{conn: conn}, nil
}

// Ping fails once the lock's connection is gone, which means the lock is released.
func (l *AdvisoryLock) Ping(ctx context.Context) error {
	if _, err := l.conn.Exec(ctx, "SELECT 1"); err != nil {
		return fmt.Errorf("db.AdvisoryLock.Ping: %w", err)
	}
	return nil
}

// Release returns the lock's connection to the pool.
func (l *AdvisoryLock) Release() {
	l.conn.Release()
}
