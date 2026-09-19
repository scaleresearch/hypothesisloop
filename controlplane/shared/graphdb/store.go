package graphdb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

var (
	// ErrNotFound is a write naming a hypothesis that does not exist.
	ErrNotFound = errors.New("hypothesis not found")
	// ErrNotOwner is a write on another agent's hypothesis. A human-submitted hypothesis has no
	// owner, so nobody gets this on one.
	ErrNotOwner = errors.New("caller does not own this hypothesis")
	// ErrInvalidLink is a link the graph refuses: bad type, unknown or cross-experiment target,
	// self link, a cycle of builds_on, or a registration with no builds_on at all.
	ErrInvalidLink = errors.New("invalid link")
	// ErrInvalidQuery is a read query Memgraph or the read-only guard refused, carrying the reason.
	ErrInvalidQuery = errors.New("invalid query")
)

// Store is the only code that speaks Cypher: every hypothesis and link lives in Memgraph and is
// read and written through here.
type Store struct {
	driver neo4j.DriverWithContext
}

// Open connects to Memgraph at url (bolt://host:7687) and verifies the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	driver, err := neo4j.NewDriverWithContext(url, neo4j.NoAuth())
	if err != nil {
		return nil, fmt.Errorf("graphdb.Open: %w", err)
	}
	if err := driver.VerifyConnectivity(ctx); err != nil {
		driver.Close(ctx) //nolint:errcheck
		return nil, fmt.Errorf("graphdb.Open: %s: %w", url, err)
	}
	return &Store{driver: driver}, nil
}

// Close releases the driver's connections.
func (s *Store) Close(ctx context.Context) error {
	return s.driver.Close(ctx)
}

// TextIndex is the full-text index over Hypothesis.text, queried with
// CALL text_search.search_all('hypothesis_text', '<words>').
const TextIndex = "hypothesis_text"

// Memgraph does not back a uniqueness constraint with an index, so every property a write looks a
// node up by gets its own.
var schemaStatements = []string{
	"CREATE CONSTRAINT ON (h:Hypothesis) ASSERT h.id IS UNIQUE",
	"CREATE CONSTRAINT ON (h:Hypothesis) ASSERT h.platform_experiment_id, h.normalized_text IS UNIQUE",
	"CREATE CONSTRAINT ON (o:Objective) ASSERT o.id IS UNIQUE",
	"CREATE INDEX ON :Hypothesis(id)",
	"CREATE INDEX ON :Hypothesis(platform_experiment_id)",
	"CREATE INDEX ON :Objective(id)",
	"CREATE TEXT INDEX " + TextIndex + " ON :Hypothesis(text)",
}

// EnsureSchema creates the constraints and indexes the store relies on. Every statement is a
// no-op when its object already exists, so it runs on every start.
func (s *Store) EnsureSchema(ctx context.Context) error {
	// Memgraph refuses schema changes inside an explicit transaction, so these run auto-commit.
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	for _, stmt := range schemaStatements {
		result, err := session.Run(ctx, stmt, nil)
		if err == nil {
			_, err = result.Consume(ctx)
		}
		if err != nil {
			return fmt.Errorf("graphdb.EnsureSchema: %s: %w", stmt, err)
		}
	}
	return nil
}

func (s *Store) write(ctx context.Context, work neo4j.ManagedTransactionWork) (any, error) {
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	return session.ExecuteWrite(ctx, work)
}

func (s *Store) read(ctx context.Context, work neo4j.ManagedTransactionWork) (any, error) {
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer session.Close(ctx)
	return session.ExecuteRead(ctx, work)
}

// collect runs one statement in tx and returns every record.
func collect(ctx context.Context, tx neo4j.ManagedTransaction, cypher string, params map[string]any) ([]*neo4j.Record, error) {
	result, err := tx.Run(ctx, cypher, params)
	if err != nil {
		return nil, err
	}
	return result.Collect(ctx)
}

// isUniqueViolation reports whether err is Memgraph refusing a commit on the named uniqueness
// constraint, e.g. ":Hypothesis(platform_experiment_id, normalized_text)".
func isUniqueViolation(err error, constraint string) bool {
	var ne *neo4j.Neo4jError
	return errors.As(err, &ne) && strings.Contains(ne.Msg, "unique constraint violation on "+constraint)
}

// now is the timestamp every write stamps. Memgraph stores microseconds, and an event's cursor is
// derived from the stored value, so truncating here keeps a live event and its replayed twin equal.
func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func str(v any) string {
	s, _ := v.(string)
	return s
}

func timeOf(v any) time.Time {
	t, _ := v.(time.Time)
	return t.UTC()
}
