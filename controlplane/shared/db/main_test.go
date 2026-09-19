package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

const devDSN = "postgres://hypothesisloop:hypothesisloop@localhost:5433/hypothesisloop?sslmode=disable"

// scratchDSN is the database TestMain creates for this run with schema.sql applied in full. Tests
// never touch the server's own databases: schema DDL there takes locks that deadlocked live
// transactions of a running stack.
var scratchDSN string

func serverDSN() string {
	if dsn := os.Getenv("HYPOTHESISLOOP_TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return devDSN
}

// testDSN is empty-safe: with no reachable server it names the server, and every test skips on
// connecting to it.
func testDSN() string {
	if scratchDSN != "" {
		return scratchDSN
	}
	return serverDSN()
}

func TestMain(m *testing.M) {
	os.Exit(runWithScratchDatabase(m))
}

func runWithScratchDatabase(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := NewPool(ctx, Config{DSN: serverDSN(), MaxConns: 1, MinConns: 1})
	if err != nil {
		return m.Run()
	}
	defer admin.Close()
	name := "db_test_" + uuid.New().String()[:8]
	if _, err := admin.pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		fmt.Fprintf(os.Stderr, "create scratch database: %v\n", err)
		return 1
	}
	defer admin.pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) //nolint:errcheck
	dsn, err := url.Parse(serverDSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse %s: %v\n", serverDSN(), err)
		return 1
	}
	dsn.Path = "/" + name
	if err := applySchemaAt(ctx, dsn.String()); err != nil {
		fmt.Fprintf(os.Stderr, "apply schema to scratch database: %v\n", err)
		return 1
	}
	scratchDSN = dsn.String()
	return m.Run()
}

func applySchemaAt(ctx context.Context, dsn string) error {
	pool, err := NewPool(ctx, Config{DSN: dsn, MaxConns: 1, MinConns: 1})
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := os.Setenv("HYPOTHESISLOOP_SCHEMA_SQL", "schema.sql"); err != nil {
		return err
	}
	defer os.Unsetenv("HYPOTHESISLOOP_SCHEMA_SQL") //nolint:errcheck
	return ApplySchema(ctx, pool)
}
