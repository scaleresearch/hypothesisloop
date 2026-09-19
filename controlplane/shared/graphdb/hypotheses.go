package graphdb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const (
	defaultListLimit = 20
	maxListLimit     = 200
	// maxLinksPerDirection bounds the links GET /hypotheses/{id} returns each way. A hypothesis
	// many others build on can have more children than that; child_count on the list item says
	// how many, and the query endpoint walks them all.
	maxLinksPerDirection = 200
)

const dedupConstraint = ":Hypothesis(platform_experiment_id, normalized_text)"

var relTypes = map[domain.LinkType]string{
	domain.LinkBuildsOn:    "BUILDS_ON",
	domain.LinkContradicts: "CONTRADICTS",
	domain.LinkRelated:     "RELATED",
}

func linkTypeOf(rel string) domain.LinkType { return domain.LinkType(strings.ToLower(rel)) }

// NewHypothesis is a registration: who, where, the claim, and where it sits in the graph.
type NewHypothesis struct {
	Source               domain.HypothesisSource
	AgentID              string
	Author               string
	PlatformExperimentID string
	Text                 string
	Links                []domain.LinkSpec
	// DistinctFrom names hypotheses the caller has read and judged a different claim, so they no
	// longer hold the registration back (see untoldSimilar).
	DistinctFrom []string
}

// Register creates a hypothesis and its outgoing links in one transaction, or returns the existing
// hypothesis with equivalent normalized text in the same platform experiment (alreadyExisted=true)
// without applying the supplied links. links are the hypothesis's outgoing links either way. It
// creates nothing and returns a *SimilarError while other hypotheses read close to the new one and
// the caller has not told it apart from them.
func (s *Store) Register(ctx context.Context, n NewHypothesis) (h *domain.Hypothesis, links []domain.Link, alreadyExisted bool, err error) {
	normalized := domain.NormalizeHypothesisText(n.Text)
	if normalized == "" {
		return nil, nil, false, fmt.Errorf("graphdb.Register: text is empty")
	}
	if err := validateLinks(n.PlatformExperimentID, n.Links, true); err != nil {
		return nil, nil, false, err
	}
	if err := s.ensureObjective(ctx, n.PlatformExperimentID); err != nil {
		return nil, nil, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, nil, false, fmt.Errorf("graphdb.Register: %w", err)
	}
	at := now()

	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		existing, err := findByNormalizedText(ctx, tx, n.PlatformExperimentID, normalized)
		if err != nil || existing != nil {
			return existing, err
		}
		if err := checkTargets(ctx, tx, n.PlatformExperimentID, "", n.Links); err != nil {
			return nil, err
		}
		similar, err := untoldSimilar(ctx, tx, n)
		if err != nil {
			return nil, err
		}
		if len(similar) > 0 {
			return nil, &SimilarError{Similar: similar}
		}
		records, err := collect(ctx, tx, `CREATE (h:Hypothesis {id: $id, platform_experiment_id: $pe, agent_id: $agent_id,
  source: $source, author: $author, text: $text, normalized_text: $normalized, status: $status, created_at: $at})
RETURN h`, map[string]any{
			"id": id.String(), "pe": n.PlatformExperimentID, "agent_id": n.AgentID, "source": string(n.Source),
			"author": n.Author, "text": n.Text, "normalized": normalized, "status": string(domain.HypothesisOpen), "at": at,
		})
		if err != nil {
			return nil, err
		}
		for _, spec := range n.Links {
			if _, err := createLink(ctx, tx, id.String(), n.PlatformExperimentID, spec, n.AgentID, n.Author, at); err != nil {
				return nil, err
			}
		}
		return hypothesisFromNode(records[0].Values[0].(neo4j.Node))
	})
	switch {
	case isUniqueViolation(err, dedupConstraint):
		// A concurrent registration of the same claim committed first.
		h, err = s.findByNormalizedText(ctx, n.PlatformExperimentID, normalized)
		if err != nil {
			return nil, nil, false, err
		}
		if h == nil {
			return nil, nil, false, fmt.Errorf("graphdb.Register: lost a dedup race but found no existing hypothesis")
		}
		alreadyExisted = true
	case err != nil:
		return nil, nil, false, wrap("graphdb.Register", err)
	default:
		h = out.(*domain.Hypothesis)
		alreadyExisted = h.ID != id.String()
	}
	links, err = s.outLinks(ctx, h.ID)
	if err != nil {
		return nil, nil, false, err
	}
	return h, links, alreadyExisted, nil
}

// LinkResult is the outcome of AddLink. Created is false when the link already existed, which is a
// no-op; CreatedAt is then zero.
type LinkResult struct {
	From      *domain.Hypothesis
	Created   bool
	CreatedAt time.Time
	Out       []domain.Link
}

// AddLink adds one outgoing link from an existing hypothesis. Only its registering agent may, or
// anyone when it was submitted by a human. Links are append-only.
func (s *Store) AddLink(ctx context.Context, fromID, agentID, author string, spec domain.LinkSpec) (*LinkResult, error) {
	at := now()
	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		from, err := getHypothesis(ctx, tx, fromID)
		if err != nil {
			return nil, err
		}
		if from == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, fromID)
		}
		if from.AgentID != "" && from.AgentID != agentID {
			return nil, fmt.Errorf("%w: hypothesis %s belongs to agent %s; register your own hypothesis that builds_on it instead", ErrNotOwner, fromID, from.AgentID)
		}
		if err := validateLinks(from.PlatformExperimentID, []domain.LinkSpec{spec}, false); err != nil {
			return nil, err
		}
		if err := checkTargets(ctx, tx, from.PlatformExperimentID, fromID, []domain.LinkSpec{spec}); err != nil {
			return nil, err
		}
		if spec.Type == domain.LinkBuildsOn && spec.To != from.PlatformExperimentID {
			if err := checkNoCycle(ctx, tx, fromID, spec.To); err != nil {
				return nil, err
			}
		}
		created, err := createLink(ctx, tx, fromID, from.PlatformExperimentID, spec, agentID, author, at)
		if err != nil {
			return nil, err
		}
		result := &LinkResult{From: from, Created: created}
		if created {
			result.CreatedAt = at
		}
		return result, nil
	})
	if err != nil {
		return nil, wrap("graphdb.AddLink", err)
	}
	result := out.(*LinkResult)
	if result.Out, err = s.outLinks(ctx, fromID); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateStatus sets a hypothesis's status on behalf of callerAgentID: the owning agent's verdict on
// its own claim, or anyone's on a human-submitted one. changed is false when the status already
// held that value, which writes nothing.
func (s *Store) UpdateStatus(ctx context.Context, id, callerAgentID string, status domain.HypothesisStatus) (h *domain.Hypothesis, changed bool, err error) {
	at := now()
	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		h, err := getHypothesis(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if h == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		if h.AgentID != "" && h.AgentID != callerAgentID {
			return nil, fmt.Errorf("%w: hypothesis %s belongs to agent %s", ErrNotOwner, id, h.AgentID)
		}
		if h.Status == status {
			return h, nil
		}
		records, err := collect(ctx, tx, `MATCH (h:Hypothesis {id: $id}) SET h.status = $status, h.status_changed_at = $at RETURN h`,
			map[string]any{"id": id, "status": string(status), "at": at})
		if err != nil {
			return nil, err
		}
		changed = true
		return hypothesisFromNode(records[0].Values[0].(neo4j.Node))
	})
	if err != nil {
		return nil, false, wrap("graphdb.UpdateStatus", err)
	}
	return out.(*domain.Hypothesis), changed, nil
}

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
		outgoing, err := linksTx(ctx, tx, `MATCH (:Hypothesis {id: $id})-[r]->(t) RETURN type(r), t, r ORDER BY r.created_at, t.id LIMIT $limit`, id)
		if err != nil {
			return nil, err
		}
		incoming, err := linksTx(ctx, tx, `MATCH (:Hypothesis {id: $id})<-[r]-(t:Hypothesis) RETURN type(r), t, r ORDER BY r.created_at, t.id LIMIT $limit`, id)
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

func (s *Store) outLinks(ctx context.Context, id string) ([]domain.Link, error) {
	links, err := s.Links(ctx, id)
	if err != nil {
		return nil, err
	}
	return links.Out, nil
}

// ListFilter scopes ListHypotheses and CountHypotheses. Empty fields do not filter; an empty
// PlatformExperimentID spans every platform experiment.
type ListFilter struct {
	PlatformExperimentID string
	AgentID              string
	Status               domain.HypothesisStatus
	Limit                int
	Offset               int
}

func (f ListFilter) where() (string, map[string]any) {
	clauses := []string{"true"}
	params := map[string]any{}
	if f.PlatformExperimentID != "" {
		clauses = append(clauses, "h.platform_experiment_id = $pe")
		params["pe"] = f.PlatformExperimentID
	}
	if f.AgentID != "" {
		clauses = append(clauses, "h.agent_id = $agent_id")
		params["agent_id"] = f.AgentID
	}
	if f.Status != "" {
		clauses = append(clauses, "h.status = $status")
		params["status"] = string(f.Status)
	}
	return strings.Join(clauses, " AND "), params
}

// itemTail turns a stream of h into list items: outgoing builds_on targets and incoming builds_on
// count, newest first.
const itemTail = `
OPTIONAL MATCH (h)-[:BUILDS_ON]->(t)
WITH h, collect(t.id) AS builds_on
OPTIONAL MATCH (c:Hypothesis)-[:BUILDS_ON]->(h)
WITH h, builds_on, count(c) AS child_count
RETURN h, builds_on, child_count
ORDER BY h.created_at DESC, h.id DESC`

// ListHypotheses returns one page of hypotheses, newest first, as list items without their
// finding/comment counts (those live in Postgres). limit <= 0 means the default, and limit is
// clamped to maxListLimit.
func (s *Store) ListHypotheses(ctx context.Context, f ListFilter) ([]*domain.HypothesisListItem, error) {
	if f.Limit <= 0 {
		f.Limit = defaultListLimit
	} else if f.Limit > maxListLimit {
		f.Limit = maxListLimit
	}
	where, params := f.where()
	params["limit"] = f.Limit
	params["offset"] = max(f.Offset, 0)
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return listItems(ctx, tx, `MATCH (h:Hypothesis) WHERE `+where+`
WITH h ORDER BY h.created_at DESC, h.id DESC SKIP $offset LIMIT $limit`+itemTail, params)
	})
	if err != nil {
		return nil, fmt.Errorf("graphdb.ListHypotheses: %w", err)
	}
	return out.([]*domain.HypothesisListItem), nil
}

// CountHypotheses returns how many hypotheses match f, ignoring Limit and Offset.
func (s *Store) CountHypotheses(ctx context.Context, f ListFilter) (int, error) {
	where, params := f.where()
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		records, err := collect(ctx, tx, `MATCH (h:Hypothesis) WHERE `+where+` RETURN count(h)`, params)
		if err != nil {
			return nil, err
		}
		return records[0].Values[0].(int64), nil
	})
	if err != nil {
		return 0, fmt.Errorf("graphdb.CountHypotheses: %w", err)
	}
	return int(out.(int64)), nil
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
RETURN a.id, b.id, type(r), r.agent_id, r.author, r.created_at ORDER BY r.created_at, a.id, b.id`, params)
		if err != nil {
			return nil, err
		}
		var d decoder
		edges := make([]domain.GraphEdge, 0, len(records))
		for _, r := range records {
			edges = append(edges, domain.GraphEdge{From: d.str(r.Values[0]), To: d.str(r.Values[1]), Type: linkTypeOf(d.str(r.Values[2])),
				AgentID: d.str(r.Values[3]), Author: d.str(r.Values[4]), CreatedAt: d.time(r.Values[5])})
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

// ImportLegacy copies hypotheses from the retired Postgres table into the graph, each building on
// its experiment's objective. Rows already present (by id) are left as they are, so a rerun after
// an interrupted migration is harmless.
func (s *Store) ImportLegacy(ctx context.Context, rows []*domain.Hypothesis) error {
	params := make([]map[string]any, 0, len(rows))
	for _, h := range rows {
		var statusChangedAt any
		if h.StatusChangedAt != nil {
			statusChangedAt = h.StatusChangedAt.UTC()
		}
		params = append(params, map[string]any{
			"id": h.ID, "pe": h.PlatformExperimentID, "agent_id": h.AgentID, "source": string(h.Source),
			"author": h.Author, "text": h.Text, "normalized": domain.NormalizeHypothesisText(h.Text),
			"status": string(h.Status), "created_at": h.CreatedAt.UTC(), "status_changed_at": statusChangedAt,
		})
	}
	_, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `UNWIND $rows AS row
MERGE (o:Objective {id: row.pe})
MERGE (h:Hypothesis {id: row.id})
ON CREATE SET h.platform_experiment_id = row.pe, h.agent_id = row.agent_id, h.source = row.source,
  h.author = row.author, h.text = row.text, h.normalized_text = row.normalized, h.status = row.status,
  h.created_at = row.created_at, h.status_changed_at = row.status_changed_at
MERGE (h)-[r:BUILDS_ON]->(o)
ON CREATE SET r.agent_id = row.agent_id, r.author = row.author, r.created_at = row.created_at`, map[string]any{"rows": params})
	})
	if err != nil {
		return fmt.Errorf("graphdb.ImportLegacy: %w", err)
	}
	return nil
}

// ensureObjective creates the objective root of a platform experiment if it is not there yet. It
// runs in its own transaction so two first registrations racing on the same root cannot fail a
// registration: the loser's commit hits the uniqueness constraint, which means the root exists.
func (s *Store) ensureObjective(ctx context.Context, platformExperimentID string) error {
	_, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `MERGE (:Objective {id: $pe})`, map[string]any{"pe": platformExperimentID})
	})
	if err != nil && !isUniqueViolation(err, ":Objective(id)") {
		return fmt.Errorf("graphdb.ensureObjective: %w", err)
	}
	return nil
}

func (s *Store) findByNormalizedText(ctx context.Context, platformExperimentID, normalized string) (*domain.Hypothesis, error) {
	out, err := s.read(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return findByNormalizedText(ctx, tx, platformExperimentID, normalized)
	})
	if err != nil {
		return nil, fmt.Errorf("graphdb.findByNormalizedText: %w", err)
	}
	return out.(*domain.Hypothesis), nil
}

func findByNormalizedText(ctx context.Context, tx neo4j.ManagedTransaction, platformExperimentID, normalized string) (*domain.Hypothesis, error) {
	return oneHypothesis(ctx, tx, `MATCH (h:Hypothesis {platform_experiment_id: $pe, normalized_text: $normalized}) RETURN h`,
		map[string]any{"pe": platformExperimentID, "normalized": normalized})
}

func getHypothesis(ctx context.Context, tx neo4j.ManagedTransaction, id string) (*domain.Hypothesis, error) {
	return oneHypothesis(ctx, tx, `MATCH (h:Hypothesis {id: $id}) RETURN h`, map[string]any{"id": id})
}

func oneHypothesis(ctx context.Context, tx neo4j.ManagedTransaction, cypher string, params map[string]any) (*domain.Hypothesis, error) {
	records, err := collect(ctx, tx, cypher, params)
	if err != nil || len(records) == 0 {
		return nil, err
	}
	return hypothesisFromNode(records[0].Values[0].(neo4j.Node))
}

// validateLinks checks what can be checked without the graph. The objective root, named by the
// platform experiment id, is only a builds_on target.
func validateLinks(platformExperimentID string, specs []domain.LinkSpec, requireBuildsOn bool) error {
	buildsOn := false
	for _, spec := range specs {
		if !domain.ValidLinkType(spec.Type) {
			return fmt.Errorf("%w: unknown link type %q, use builds_on, contradicts or related", ErrInvalidLink, spec.Type)
		}
		if spec.To == "" {
			return fmt.Errorf("%w: a %s link needs a target in `to`", ErrInvalidLink, spec.Type)
		}
		if spec.To == platformExperimentID && spec.Type != domain.LinkBuildsOn {
			return fmt.Errorf("%w: the objective (%s) can only be a builds_on target; %s links connect two hypotheses", ErrInvalidLink, platformExperimentID, spec.Type)
		}
		buildsOn = buildsOn || spec.Type == domain.LinkBuildsOn
	}
	if requireBuildsOn && !buildsOn {
		return fmt.Errorf("%w: at least one builds_on link is required: build on the objective (to = %s) or on existing hypotheses; find where your idea belongs with POST /hypotheses/query first", ErrInvalidLink, platformExperimentID)
	}
	return nil
}

// checkTargets verifies every hypothesis a link points at exists, belongs to platformExperimentID
// and is not fromID itself.
func checkTargets(ctx context.Context, tx neo4j.ManagedTransaction, platformExperimentID, fromID string, specs []domain.LinkSpec) error {
	var ids []string
	for _, spec := range specs {
		if spec.To == fromID {
			return fmt.Errorf("%w: a hypothesis cannot link to itself", ErrInvalidLink)
		}
		if spec.To != platformExperimentID && !slices.Contains(ids, spec.To) {
			ids = append(ids, spec.To)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	records, err := collect(ctx, tx, `UNWIND $ids AS id MATCH (t:Hypothesis {id: id}) RETURN t.id, t.platform_experiment_id`, map[string]any{"ids": ids})
	if err != nil {
		return err
	}
	var d decoder
	found := make(map[string]string, len(records))
	for _, r := range records {
		found[d.str(r.Values[0])] = d.str(r.Values[1])
	}
	if d.err != nil {
		return d.err
	}
	for _, id := range ids {
		pe, ok := found[id]
		if !ok {
			return fmt.Errorf("%w: no hypothesis %q; a link targets an existing hypothesis id in platform experiment %s, or %s itself for the objective", ErrInvalidLink, id, platformExperimentID, platformExperimentID)
		}
		if pe != platformExperimentID {
			return fmt.Errorf("%w: hypothesis %s belongs to platform experiment %s; links stay within %s", ErrInvalidLink, id, pe, platformExperimentID)
		}
	}
	return nil
}

// checkNoCycle refuses fromID builds_on toID when toID already builds on fromID, directly or
// through other hypotheses.
func checkNoCycle(ctx context.Context, tx neo4j.ManagedTransaction, fromID, toID string) error {
	records, err := collect(ctx, tx, `MATCH (t:Hypothesis {id: $to}), (f:Hypothesis {id: $from})
MATCH p = (t)-[:BUILDS_ON *BFS]->(f) RETURN count(p)`, map[string]any{"from": fromID, "to": toID})
	if err != nil {
		return err
	}
	if len(records) > 0 && records[0].Values[0].(int64) > 0 {
		return fmt.Errorf("%w: %s already builds on %s, so %s builds_on %s would make a cycle", ErrInvalidLink, toID, fromID, fromID, toID)
	}
	return nil
}

// createLink merges one edge from fromID, reporting whether it was new. The relationship type is
// spliced into the statement because Cypher cannot parameterise it; it only ever comes from
// relTypes, after validateLinks.
func createLink(ctx context.Context, tx neo4j.ManagedTransaction, fromID, platformExperimentID string, spec domain.LinkSpec, agentID, author string, at time.Time) (bool, error) {
	target := "(t:Hypothesis {id: $to})"
	if spec.To == platformExperimentID {
		target = "(t:Objective {id: $to})"
	}
	records, err := collect(ctx, tx, `MATCH (h:Hypothesis {id: $from}), `+target+`
MERGE (h)-[r:`+relTypes[spec.Type]+`]->(t)
ON CREATE SET r.agent_id = $agent_id, r.author = $author, r.created_at = $at
RETURN r.created_at = $at`, map[string]any{"from": fromID, "to": spec.To, "agent_id": agentID, "author": author, "at": at})
	if err != nil {
		return false, err
	}
	if len(records) == 0 {
		return false, fmt.Errorf("%w: link target %s vanished", ErrInvalidLink, spec.To)
	}
	created, _ := records[0].Values[0].(bool)
	return created, nil
}

func linksTx(ctx context.Context, tx neo4j.ManagedTransaction, cypher, id string) ([]domain.Link, error) {
	records, err := collect(ctx, tx, cypher, map[string]any{"id": id, "limit": maxLinksPerDirection})
	if err != nil {
		return nil, err
	}
	var d decoder
	links := make([]domain.Link, 0, len(records))
	for _, r := range records {
		t := r.Values[1].(neo4j.Node)
		rel := r.Values[2].(neo4j.Relationship)
		link := domain.Link{
			Type:         linkTypeOf(d.str(r.Values[0])),
			HypothesisID: d.str(t.Props["id"]),
			AgentID:      d.str(rel.Props["agent_id"]),
			Author:       d.str(rel.Props["author"]),
			CreatedAt:    d.time(rel.Props["created_at"]),
		}
		if slices.Contains(t.Labels, "Hypothesis") {
			link.Text = d.str(t.Props["text"])
			link.Status = d.str(t.Props["status"])
		}
		links = append(links, link)
	}
	return links, d.err
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

func hypothesisFromNode(n neo4j.Node) (*domain.Hypothesis, error) {
	p := n.Props
	var d decoder
	h := &domain.Hypothesis{
		ID:                   d.str(p["id"]),
		AgentID:              d.str(p["agent_id"]),
		Source:               domain.HypothesisSource(d.str(p["source"])),
		Author:               d.str(p["author"]),
		PlatformExperimentID: d.str(p["platform_experiment_id"]),
		Text:                 d.str(p["text"]),
		Status:               domain.HypothesisStatus(d.str(p["status"])),
		CreatedAt:            d.time(p["created_at"]),
	}
	if v, ok := p["status_changed_at"]; ok && v != nil {
		changed := d.time(v)
		h.StatusChangedAt = &changed
	}
	if d.err != nil {
		return nil, fmt.Errorf("hypothesis %v: %w", p["id"], d.err)
	}
	return h, nil
}

// wrap prefixes err with op unless it is one of this package's own sentinel errors, whose message
// is already written for the caller to act on.
func wrap(op string, err error) error {
	for _, sentinel := range []error{ErrNotFound, ErrNotOwner, ErrInvalidLink, ErrSimilar} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
