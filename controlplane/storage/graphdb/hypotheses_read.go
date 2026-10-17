package graphdb

import (
	"context"
	"fmt"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// GetHypothesis returns one hypothesis, or nil when there is none with that id.
func (s *Store) GetHypothesis(ctx context.Context, id string) (*domain.Hypothesis, error) {
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return getHypothesis(ctx, tx, id)
	})
	if err != nil {
		return nil, fmt.Errorf("graphdb.GetHypothesis: %w", err)
	}
	return out.(*domain.Hypothesis), nil
}

// Links returns the links touching one hypothesis, oldest first, at most maxLinksPerDirection each
// way. Incoming links only ever come from hypotheses; outgoing builds_on may reach the objective.
func (s *Store) Links(ctx context.Context, id string) (domain.HypothesisLinks, error) {
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		outgoing, err := linksTx(ctx, tx, `MATCH (:Hypothesis {id: $id})-[r]->(t) RETURN type(r), t, r ORDER BY `+linkOrder+` LIMIT $limit`, id)
		if err != nil {
			return nil, err
		}
		incoming, err := linksTx(ctx, tx, incomingLinks, id)
		if err != nil {
			return nil, err
		}
		return domain.HypothesisLinks{Out: outgoing, In: incoming}, nil
	})
	if err != nil {
		return domain.HypothesisLinks{}, fmt.Errorf("graphdb.Links: %w", err)
	}
	return out.(domain.HypothesisLinks), nil
}

const incomingLinks = `MATCH (:Hypothesis {id: $id})<-[r]-(t:Hypothesis) RETURN type(r), t, r ORDER BY r.created_at, t.id LIMIT $limit`

// linkOrder is the order of a hypothesis's outgoing links r to t: oldest first, and those one
// write stated in the order it stated them. A link from before that order was kept has no
// ordinal and sorts by its target.
const linkOrder = "r.created_at, coalesce(r.ordinal, 0), t.id"

// FirstParent returns the target of a hypothesis's first builds_on link (linkOrder): a
// hypothesis id, or the platform experiment id for the objective. Empty when there is none.
func (s *Store) FirstParent(ctx context.Context, id string) (string, error) {
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `MATCH (:Hypothesis {id: $id})-[r:BUILDS_ON]->(t) RETURN t.id ORDER BY `+linkOrder+` LIMIT 1`, map[string]any{"id": id})
	})
	if err != nil {
		return "", fmt.Errorf("graphdb.FirstParent: %w", err)
	}
	records := out.([]*neo4j.Record)
	if len(records) == 0 {
		return "", nil
	}
	var d decoder
	parent := d.str(records[0].Values[0])
	if d.err != nil {
		return "", fmt.Errorf("graphdb.FirstParent: %w", d.err)
	}
	return parent, nil
}

func (s *Store) outLinks(ctx context.Context, id string) ([]domain.Link, error) {
	links, err := s.Links(ctx, id)
	if err != nil {
		return nil, err
	}
	return links.Out, nil
}

// ListFilter scopes ListHypotheses and HypothesisStats. Empty fields do not filter; an empty
// PlatformExperimentID spans every platform experiment.
type ListFilter struct {
	PlatformExperimentID string
	AgentID              string
	Status               domain.HypothesisStatus
	// Within, when Restricted, keeps only hypotheses of these platform experiments: the caller's
	// scope, which this store cannot derive because it holds no orgs or projects.
	Restricted bool
	Within     []string
	// ByPeople keeps only the hypotheses a person registered.
	ByPeople bool
	// Search keeps only the hypotheses whose text holds it, ignoring case.
	Search string
	// IDs, when not nil, keeps only these hypotheses.
	IDs []string
	// ExcludeIDs, when not nil, drops these hypotheses.
	ExcludeIDs []string
	Limit      int
	Offset     int
}

// personIdea and openPersonIdea say, of h, that a person registered it, and that it is a person's
// idea no agent has settled or built on yet: still open, with no hypothesis an agent registered
// building on it.
var (
	personIdea     = `h.actor_kind IN ['` + string(domain.ActorUser) + `', '` + string(domain.ActorKey) + `', '` + string(domain.ActorStaff) + `']`
	openPersonIdea = `(` + personIdea + ` AND h.status = '` + string(domain.HypothesisOpen) +
		`' AND NOT exists((h)<-[:BUILDS_ON]-(:Hypothesis {actor_kind: '` + string(domain.ActorAgent) + `'})))`
)

// listOrder puts the open ideas of people first, then the newest.
var listOrder = `ORDER BY ` + openPersonIdea + ` DESC, h.created_at DESC, h.id DESC`

func (f ListFilter) where() (string, map[string]any) {
	clauses := []string{"true"}
	params := map[string]any{}
	if f.PlatformExperimentID != "" {
		clauses = append(clauses, "h.platform_experiment_id = $pe")
		params["pe"] = f.PlatformExperimentID
	}
	if f.Restricted {
		clauses = append(clauses, "h.platform_experiment_id IN $within")
		params["within"] = f.Within
	}
	if f.AgentID != "" {
		clauses = append(clauses, "h.agent_id = $agent_id")
		params["agent_id"] = f.AgentID
	}
	if f.Status != "" {
		clauses = append(clauses, "h.status = $status")
		params["status"] = string(f.Status)
	}
	if f.ByPeople {
		clauses = append(clauses, personIdea)
	}
	if f.Search != "" {
		clauses = append(clauses, "toLower(h.text) CONTAINS toLower($search)")
		params["search"] = f.Search
	}
	if f.IDs != nil {
		clauses = append(clauses, "h.id IN $ids")
		params["ids"] = f.IDs
	}
	if f.ExcludeIDs != nil {
		clauses = append(clauses, "NOT h.id IN $exclude")
		params["exclude"] = f.ExcludeIDs
	}
	return strings.Join(clauses, " AND "), params
}

// itemTail turns a stream of h into list items: outgoing builds_on targets and incoming builds_on
// count, in listOrder.
var itemTail = `
OPTIONAL MATCH (h)-[:BUILDS_ON]->(t)
WITH h, collect(t.id) AS builds_on
OPTIONAL MATCH (c:Hypothesis)-[:BUILDS_ON]->(h)
WITH h, builds_on, count(c) AS child_count
RETURN h, builds_on, child_count
` + listOrder

// ListHypotheses returns one page of hypotheses, the open ideas of people first, then newest
// first, as list items without their
// finding/comment counts (those live in Postgres). The caller states the page: a limit outside 1
// to maxListLimit or a negative offset is an error, so a short page is never read as the whole
// set.
func (s *Store) ListHypotheses(ctx context.Context, f ListFilter) ([]*domain.HypothesisListItem, error) {
	if f.Limit < 1 || f.Limit > maxListLimit || f.Offset < 0 {
		return nil, fmt.Errorf("graphdb.ListHypotheses: limit %d and offset %d are outside 1 to %d and 0 up", f.Limit, f.Offset, maxListLimit)
	}
	where, params := f.where()
	params["limit"] = f.Limit
	params["offset"] = f.Offset
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return listItems(ctx, tx, `MATCH (h:Hypothesis) WHERE `+where+`
WITH h `+listOrder+` SKIP $offset LIMIT $limit`+itemTail, params)
	})
	if err != nil {
		return nil, fmt.Errorf("graphdb.ListHypotheses: %w", err)
	}
	return out.([]*domain.HypothesisListItem), nil
}

// HypothesisStats counts the hypotheses matching f by status, ignoring Limit and Offset.
func (s *Store) HypothesisStats(ctx context.Context, f ListFilter) (*domain.HypothesisStats, error) {
	where, params := f.where()
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `MATCH (h:Hypothesis) WHERE `+where+` RETURN h.status, count(h)`, params)
	})
	if err != nil {
		return nil, fmt.Errorf("graphdb.HypothesisStats: %w", err)
	}
	stats := &domain.HypothesisStats{ByStatus: map[domain.HypothesisStatus]int{}}
	for _, record := range out.([]*neo4j.Record) {
		n := int(record.Values[1].(int64))
		stats.ByStatus[domain.HypothesisStatus(record.Values[0].(string))] = n
		stats.Total += n
	}
	return stats, nil
}

// OpenPersonIdeas counts the ideas people registered in a platform experiment that no agent has
// settled or built on yet.
func (s *Store) OpenPersonIdeas(ctx context.Context, platformExperimentID string) (int, error) {
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `MATCH (h:Hypothesis {platform_experiment_id: $pe}) WHERE `+openPersonIdea+` RETURN count(h)`,
			map[string]any{"pe": platformExperimentID})
	})
	if err != nil {
		return 0, fmt.Errorf("graphdb.OpenPersonIdeas: %w", err)
	}
	var d decoder
	n := d.int(out.([]*neo4j.Record)[0].Values[0])
	if d.err != nil {
		return 0, fmt.Errorf("graphdb.OpenPersonIdeas: %w", d.err)
	}
	return int(n), nil
}

// ExperimentGraph returns the whole hypothesis graph of one platform experiment: every hypothesis as a list
// item and every edge leaving one. An edge to the objective has To = platformExperimentID.
func (s *Store) ExperimentGraph(ctx context.Context, platformExperimentID string) ([]*domain.HypothesisListItem, []domain.GraphEdge, error) {
	type graph struct {
		nodes []*domain.HypothesisListItem
		edges []domain.GraphEdge
	}
	params := map[string]any{"pe": platformExperimentID}
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		nodes, err := listItems(ctx, tx, `MATCH (h:Hypothesis {platform_experiment_id: $pe}) WITH h`+itemTail, params)
		if err != nil {
			return nil, err
		}
		records, err := collect(ctx, tx, `MATCH (a:Hypothesis {platform_experiment_id: $pe})-[r]->(b)
RETURN a.id, b.id, type(r), r.agent_id, coalesce(r.actor_kind, 'unverified'), coalesce(r.actor_id, ''), coalesce(r.author, ''), r.created_at ORDER BY r.created_at, a.id, b.id`, params)
		if err != nil {
			return nil, err
		}
		var d decoder
		edges := make([]domain.GraphEdge, 0, len(records))
		for _, r := range records {
			edge := domain.GraphEdge{From: d.str(r.Values[0]), To: d.str(r.Values[1]), Type: linkTypeOf(d.str(r.Values[2])),
				AgentID: d.str(r.Values[3]), CreatedAt: d.time(r.Values[7])}
			edge.Actor = domain.StoredActor(domain.ActorKind(d.str(r.Values[4])), d.str(r.Values[5]), d.str(r.Values[6]), edge.AgentID)
			edges = append(edges, edge)
		}
		if d.err != nil {
			return nil, d.err
		}
		return graph{nodes: nodes, edges: edges}, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("graphdb.ExperimentGraph: %w", err)
	}
	g := out.(graph)
	return g.nodes, g.edges, nil
}

func listItems(ctx context.Context, tx neo4j.ManagedTransaction, cypher string, params map[string]any) ([]*domain.HypothesisListItem, error) {
	records, err := collect(ctx, tx, cypher, params)
	if err != nil {
		return nil, err
	}
	var d decoder
	items := make([]*domain.HypothesisListItem, 0, len(records))
	for _, r := range records {
		h, err := hypothesisFromNode(r.Values[0].(neo4j.Node))
		if err != nil {
			return nil, err
		}
		item := &domain.HypothesisListItem{Hypothesis: h, BuildsOn: []string{}}
		for _, id := range r.Values[1].([]any) {
			item.BuildsOn = append(item.BuildsOn, d.str(id))
		}
		item.ChildCount = int(r.Values[2].(int64))
		items = append(items, item)
	}
	return items, d.err
}
