package graphdb

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// ConcernsAgent reports, per event, whether a hypothesis event concerns a hypothesis agentID
// registered: the subject of a comment, finding or status, the target of a link, or a target of
// the links a new hypothesis was registered with. A comment on the objective, whose subject is the
// platform experiment's id, concerns every agent: the objective is everyone's. A link added to a
// new hypothesis later is its own event, so it does not make the hypothesis.new concern the agent.
// It is one read however many events are asked about.
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
