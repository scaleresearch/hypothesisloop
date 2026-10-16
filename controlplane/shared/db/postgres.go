package db

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
)

// ServiceMaxConns is the pool size every control-plane service runs with.
const ServiceMaxConns = 20

// ConfigOf is the pool of maxConns connections the database section of the settings describes.
func ConfigOf(d config.DatabaseConfig, maxConns int32) Config {
	return Config{DSN: d.URL, MaxConns: maxConns, Timeouts: Timeouts{
		Statement:         time.Duration(d.StatementTimeoutSeconds) * time.Second,
		Lock:              time.Duration(d.LockTimeoutSeconds) * time.Second,
		IdleInTransaction: time.Duration(d.IdleInTransactionSessionTimeoutSeconds) * time.Second,
	}}
}

// Config holds the database connection configuration. MaxConns and every timeout are required.
type Config struct {
	DSN      string
	MaxConns int32
	MinConns int32
	Timeouts Timeouts
}

// Timeouts bound what one session may hold: one statement, one wait for a lock, and a
// transaction left idle. Postgres enforces them, so a statement stuck behind a lock or a
// transaction abandoned mid-way frees its connection and its locks on its own.
type Timeouts struct {
	Statement         time.Duration
	Lock              time.Duration
	IdleInTransaction time.Duration
}

func (t Timeouts) params() (map[string]string, error) {
	params := map[string]string{
		"statement_timeout":                   strconv.FormatInt(t.Statement.Milliseconds(), 10),
		"lock_timeout":                        strconv.FormatInt(t.Lock.Milliseconds(), 10),
		"idle_in_transaction_session_timeout": strconv.FormatInt(t.IdleInTransaction.Milliseconds(), 10),
	}
	for name, ms := range params {
		if n, _ := strconv.ParseInt(ms, 10, 64); n <= 0 {
			return nil, fmt.Errorf("%s must be at least a millisecond", name)
		}
	}
	return params, nil
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

	params, err := cfg.Timeouts.params()
	if err != nil {
		return nil, fmt.Errorf("db.NewPool: %w", err)
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db.NewPool: parse config: %w", err)
	}
	for name, value := range params {
		poolCfg.ConnConfig.RuntimeParams[name] = value
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

// Release closes the lock's connection, which is what releases the lock: a connection handed
// back to the pool after a ping that did not answer could still hold it.
func (l *AdvisoryLock) Release() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l.conn.Conn().Close(ctx) //nolint:errcheck
	l.conn.Release()
}
