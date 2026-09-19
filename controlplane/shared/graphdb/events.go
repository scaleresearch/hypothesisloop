package graphdb

import (
	"context"
	"fmt"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
)

// ReplayEvents derives the hypothesis events of one platform experiment that happened after since,
// from the timestamps the graph already holds: hypothesis.new from a node's created_at,
// hypothesis.status from its status_changed_at, hypothesis.link from an edge's created_at. Edges
// created with their hypothesis share its created_at and are skipped, as hypothesis.new announced
// them. Like every replay it reports the latest status, not each one passed through.
func (s *Store) ReplayEvents(ctx context.Context, platformExperimentID string, since time.Time) ([]db.Event, error) {
	params := map[string]any{"pe": platformExperimentID, "since": since.UTC()}
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		var events []db.Event
		for _, q := range []string{
			`MATCH (h:Hypothesis {platform_experiment_id: $pe}) WHERE h.created_at > $since
RETURN 'hypothesis.new' AS kind, h.id AS subject, h.source AS value, '' AS detail, h.agent_id AS agent_id, h.created_at AS at`,
			`MATCH (h:Hypothesis {platform_experiment_id: $pe}) WHERE h.status_changed_at > $since
RETURN 'hypothesis.status' AS kind, h.id AS subject, h.status AS value, '' AS detail, h.agent_id AS agent_id, h.status_changed_at AS at`,
			`MATCH (h:Hypothesis {platform_experiment_id: $pe})-[r]->(t) WHERE r.created_at > $since AND r.created_at <> h.created_at
RETURN 'hypothesis.link' AS kind, h.id AS subject, toLower(type(r)) AS value, t.id AS detail, r.agent_id AS agent_id, r.created_at AS at`,
		} {
			records, err := collect(ctx, tx, q, params)
			if err != nil {
				return nil, err
			}
			for _, r := range records {
				events = append(events, db.Event{
					Kind:                 str(r.Values[0]),
					Subject:              str(r.Values[1]),
					Value:                str(r.Values[2]),
					Detail:               str(r.Values[3]),
					PlatformExperimentID: platformExperimentID,
					AgentID:              str(r.Values[4]),
					Cursor:               db.NewCursor(timeOf(r.Values[5])),
				})
			}
		}
		return events, nil
	})
	if err != nil {
		return nil, fmt.Errorf("graphdb.ReplayEvents: %w", err)
	}
	return out.([]db.Event), nil
}
