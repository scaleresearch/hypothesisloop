package graphdb

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	// Memgraph returns stored timestamps with their zone name (Etc/UTC) and the driver resolves it
	// with time.LoadLocation, which fails in an image without a zone database.
	_ "time/tzdata"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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
	// ErrDuplicateText is an edit to the text another hypothesis of the platform experiment states.
	ErrDuplicateText = errors.New("another hypothesis of this platform experiment states the same text")
)

// LinkedError refuses an edit or removal of a hypothesis other hypotheses link to. In is those
// links, at most maxLinksPerDirection.
type LinkedError struct {
	In []domain.Link
}

func (e *LinkedError) Error() string {
	return fmt.Sprintf("%d hypotheses link to this one", len(e.In))
}

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

// schemaObject is a constraint or an index the store relies on: the statement that creates it,
// and the line it is listed as (listedSchema).
type schemaObject struct {
	create, listed string
}

// Memgraph does not back a uniqueness constraint with an index, so every property a write looks a
// node up by gets its own.
var schema = []schemaObject{
	{"CREATE CONSTRAINT ON (h:Hypothesis) ASSERT h.id IS UNIQUE", "constraint unique Hypothesis(id)"},
	{"CREATE CONSTRAINT ON (h:Hypothesis) ASSERT h.platform_experiment_id, h.normalized_text IS UNIQUE",
		"constraint unique Hypothesis(platform_experiment_id, normalized_text)"},
	{"CREATE CONSTRAINT ON (o:Objective) ASSERT o.id IS UNIQUE", "constraint unique Objective(id)"},
	{"CREATE INDEX ON :Hypothesis(id)", "index label+property Hypothesis(id)"},
	{"CREATE INDEX ON :Hypothesis(platform_experiment_id)", "index label+property Hypothesis(platform_experiment_id)"},
	{"CREATE INDEX ON :Objective(id)", "index label+property Objective(id)"},
	{"CREATE TEXT INDEX " + TextIndex + " ON :Hypothesis(text)", "index label_text (name: " + TextIndex + ") Hypothesis(text)"},
}

// EnsureSchema creates the constraints and indexes the store relies on, then refuses unless they
// are exactly the ones the store holds. Every statement is a no-op when its object already
// exists, so it runs on every start.
func (s *Store) EnsureSchema(ctx context.Context) error {
	// Memgraph refuses schema changes inside an explicit transaction, so these run auto-commit.
	session := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
	defer session.Close(ctx)
	want := map[string]bool{}
	for _, o := range schema {
		result, err := session.Run(ctx, o.create, nil)
		if err == nil {
			_, err = result.Consume(ctx)
		}
		if err != nil {
			return fmt.Errorf("graphdb.EnsureSchema: %s: %w", o.create, err)
		}
		want[o.listed] = true
	}
	have, err := listedSchema(ctx, session)
	if err != nil {
		return fmt.Errorf("graphdb.EnsureSchema: %w", err)
	}
	var differences []string
	for _, o := range schema {
		if !have[o.listed] {
			differences = append(differences, "missing "+o.listed)
		}
	}
	for _, listed := range slices.Sorted(maps.Keys(have)) {
		if !want[listed] {
			differences = append(differences, "unexpected "+listed)
		}
	}
	if len(differences) > 0 {
		return fmt.Errorf("graphdb.EnsureSchema: the graph is not the schema this release declares: %s", strings.Join(differences, "; "))
	}
	return nil
}

// listedSchema is every index and constraint the store holds, each as "index <type>
// <label>(<properties>)" or "constraint <type> <label>(<properties>)".
func listedSchema(ctx context.Context, session neo4j.SessionWithContext) (map[string]bool, error) {
	listed := map[string]bool{}
	for _, show := range []struct{ query, kind, typeKey, propertiesKey string }{
		{"SHOW INDEX INFO", "index", "index type", "property"},
		{"SHOW CONSTRAINT INFO", "constraint", "constraint type", "properties"},
	} {
		result, err := session.Run(ctx, show.query, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", show.query, err)
		}
		records, err := result.Collect(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", show.query, err)
		}
		for _, r := range records {
			kind, _ := r.Get(show.typeKey)
			label, _ := r.Get("label")
			raw, _ := r.Get(show.propertiesKey)
			names, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("%s: %s is %T, want a list", show.query, show.propertiesKey, raw)
			}
			properties := make([]string, len(names))
			for i, name := range names {
				properties[i] = fmt.Sprint(name)
			}
			listed[fmt.Sprintf("%s %v %v(%s)", show.kind, kind, label, strings.Join(properties, ", "))] = true
		}
	}
	return listed, nil
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

// now is the timestamp every write stamps. Memgraph stores microseconds, and writes compare the
// stored value with this one, so it is truncated to match.
func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// decoder reads stored values and keeps the first one of the wrong type as err, so a bad property
// fails the read that met it.
type decoder struct {
	err error
}

func (d *decoder) str(v any) string {
	s, ok := v.(string)
	if !ok && d.err == nil {
		d.err = fmt.Errorf("graphdb: want a string, got %T", v)
	}
	return s
}

func (d *decoder) int(v any) int64 {
	i, ok := v.(int64)
	if !ok && d.err == nil {
		d.err = fmt.Errorf("graphdb: want an integer, got %T", v)
	}
	return i
}

func (d *decoder) time(v any) time.Time {
	t, ok := v.(time.Time)
	if !ok && d.err == nil {
		d.err = fmt.Errorf("graphdb: want a timestamp, got %T", v)
	}
	return t.UTC()
}
