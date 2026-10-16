package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/agenttypes"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const devDSN = "postgres://hypothesisloop:hypothesisloop@localhost:5433/hypothesisloop?sslmode=disable"

// scratchDSN is the database TestMain creates for this run with every migration applied. Tests
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
	if err := migrateAt(ctx, dsn.String()); err != nil {
		fmt.Fprintf(os.Stderr, "migrate scratch database: %v\n", err)
		return 1
	}
	scratchDSN = dsn.String()
	return m.Run()
}

func migrateAt(ctx context.Context, dsn string) error {
	pool, err := NewPool(ctx, Config{DSN: dsn, MaxConns: 1, MinConns: 1})
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := Migrate(ctx, pool); err != nil {
		return err
	}
	builtins, err := agenttypes.Builtins()
	if err != nil {
		return err
	}
	_, err = SeedBuiltins(ctx, pool, agenttypes.Release, builtins, testRules)
	return err
}

// testModelDefaults is the platform's default models in the tests of this package.
var testModelDefaults = domain.ModelDefaults{Agent: "agent-default", Coordinator: "coordinator-default"}

// testRules is the platform's rules the tests of this package seed.
const testRules = "## platform rules (read first)"
