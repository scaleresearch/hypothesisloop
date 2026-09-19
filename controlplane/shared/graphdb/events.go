package graphdb

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
)

// ReplayEvents derives the hypothesis events of one platform experiment at a position of at least
// since, from the positions the graph already holds: hypothesis.new from a node's created_xid,
// hypothesis.status from its status_changed_xid, hypothesis.link from an edge's created_xid. Edges
// created with their hypothesis share its created_at and are skipped, as hypothesis.new announced
// them. Like every replay it reports the latest status, not each one passed through.
func (s *Store) ReplayEvents(ctx context.Context, platformExperimentID string, since int64) ([]db.Event, error) {
	params := map[string]any{"pe": platformExperimentID, "since": since}
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		var events []db.Event
		for _, q := range []string{
			`MATCH (h:Hypothesis {platform_experiment_id: $pe}) WHERE h.created_xid >= $since
RETURN 'hypothesis.new' AS kind, h.id AS subject, h.source AS value, '' AS detail, h.agent_id AS agent_id, h.created_xid AS position`,
			`MATCH (h:Hypothesis {platform_experiment_id: $pe}) WHERE h.status_changed_xid >= $since
RETURN 'hypothesis.status' AS kind, h.id AS subject, h.status AS value, '' AS detail, h.agent_id AS agent_id, h.status_changed_xid AS position`,
			`MATCH (h:Hypothesis {platform_experiment_id: $pe})-[r]->(t) WHERE r.created_xid >= $since AND r.created_at <> h.created_at
RETURN 'hypothesis.link' AS kind, h.id AS subject, toLower(type(r)) AS value, t.id AS detail, r.agent_id AS agent_id, r.created_xid AS position`,
		} {
			records, err := collect(ctx, tx, q, params)
			if err != nil {
				return nil, err
			}
			var d decoder
			for _, r := range records {
				events = append(events, db.Event{
					Kind:                 d.str(r.Values[0]),
					Subject:              d.str(r.Values[1]),
					Value:                d.str(r.Values[2]),
					Detail:               d.str(r.Values[3]),
					PlatformExperimentID: platformExperimentID,
					AgentID:              d.str(r.Values[4]),
					Position:             d.int(r.Values[5]),
				})
			}
			if d.err != nil {
				return nil, d.err
			}
		}
		return events, nil
	})
	if err != nil {
		return nil, fmt.Errorf("graphdb.ReplayEvents: %w", err)
	}
	return out.([]db.Event), nil
}

// ConcernsAgent reports whether a hypothesis event concerns a hypothesis agentID registered: the
// subject of a comment, finding or status, the target of a link, or a target of the links a new
// hypothesis was registered with. A link added to a new hypothesis later is its own event, so it
// does not make the hypothesis.new concern the agent, live or in a replay.
func (s *Store) ConcernsAgent(ctx context.Context, agentID string, e db.Event) (bool, error) {
	q, id := `MATCH (t:Hypothesis {id: $id, agent_id: $agent}) RETURN count(t) > 0`, e.Subject
	switch e.Kind {
	case db.EventHypothesisNew:
		q = `MATCH (h:Hypothesis {id: $id})-[r]->(t:Hypothesis {agent_id: $agent}) WHERE r.created_at = h.created_at
RETURN count(t) > 0`
	case db.EventHypothesisLink:
		id = e.Detail
	}
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		records, err := collect(ctx, tx, q, map[string]any{"id": id, "agent": agentID})
		if err != nil {
			return nil, err
		}
		return records[0].Values[0].(bool), nil
	})
	if err != nil {
		return false, fmt.Errorf("graphdb.ConcernsAgent: %w", err)
	}
	return out.(bool), nil
}
