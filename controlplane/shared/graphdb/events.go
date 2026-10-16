package graphdb

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// ReplayEvents derives the hypothesis events of one platform experiment at a position of at least
// since, from the positions the graph already holds: hypothesis.new from a node's created_xid,
// with the kind of who registered it,
// hypothesis.status from its status_changed_xid, hypothesis.link from an edge's created_xid. Edges
// created with their hypothesis share its created_at and are skipped, as hypothesis.new announced
// them. Like every replay it reports the latest status, not each one passed through.
func (s *Store) ReplayEvents(ctx context.Context, platformExperimentID string, since int64) ([]domain.Event, error) {
	params := map[string]any{"pe": platformExperimentID, "since": since}
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		var events []domain.Event
		for _, q := range []string{
			`MATCH (h:Hypothesis {platform_experiment_id: $pe}) WHERE h.created_xid >= $since
RETURN 'hypothesis.new' AS kind, h.id AS subject, coalesce(h.actor_kind, '') AS value, '' AS detail, h.agent_id AS agent_id, h.created_xid AS position`,
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
				events = append(events, domain.Event{
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
	return out.([]domain.Event), nil
}

// ConcernsAgent reports, per event, whether a hypothesis event concerns a hypothesis agentID
// registered: the subject of a comment, finding or status, the target of a link, or a target of
// the links a new hypothesis was registered with. A comment on the objective, whose subject is the
// platform experiment's id, concerns every agent: the objective is everyone's. A link added to a
// new hypothesis later is its own event, so it does not make the hypothesis.new concern the agent,
// live or in a replay. It is one read however many events are asked about.
func (s *Store) ConcernsAgent(ctx context.Context, agentID string, events []domain.Event) ([]bool, error) {
	var targets, created []string
	for _, e := range events {
		switch {
		case onObjective(e):
		case e.Kind == domain.EventHypothesisNew:
			created = append(created, e.Subject)
		case e.Kind == domain.EventHypothesisLink:
			targets = append(targets, e.Detail)
		default:
			targets = append(targets, e.Subject)
		}
	}
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		concerned := map[string]bool{}
		for _, q := range []struct {
			cypher string
			ids    []string
		}{
			{`MATCH (t:Hypothesis {agent_id: $agent}) WHERE t.id IN $ids RETURN t.id`, targets},
			{`MATCH (h:Hypothesis)-[r]->(t:Hypothesis {agent_id: $agent}) WHERE h.id IN $ids AND r.created_at = h.created_at
RETURN DISTINCT h.id`, created},
		} {
			if len(q.ids) == 0 {
				continue
			}
			records, err := collect(ctx, tx, q.cypher, map[string]any{"ids": q.ids, "agent": agentID})
			if err != nil {
				return nil, err
			}
			var d decoder
			for _, r := range records {
				concerned[d.str(r.Values[0])] = true
			}
			if d.err != nil {
				return nil, d.err
			}
		}
		return concerned, nil
	})
	if err != nil {
		return nil, fmt.Errorf("graphdb.ConcernsAgent: %w", err)
	}
	concerned := out.(map[string]bool)
	result := make([]bool, len(events))
	for i, e := range events {
		id := e.Subject
		if e.Kind == domain.EventHypothesisLink {
			id = e.Detail
		}
		result[i] = onObjective(e) || concerned[id]
	}
	return result, nil
}

// onObjective is a comment on the objective, the root of its platform experiment's graph.
func onObjective(e domain.Event) bool {
	return e.Kind == domain.EventCommentNew && e.Subject != "" && e.Subject == e.PlatformExperimentID
}
