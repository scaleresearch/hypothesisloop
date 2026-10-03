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

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const (
	maxListLimit = 200
	// maxLinksPerDirection bounds the links GET /hypotheses/{id} returns each way. A hypothesis
	// many others build on can have more children than that; child_count on the list item says
	// how many, and the query endpoint walks them all.
	maxLinksPerDirection = 200
)

var relTypes = map[domain.LinkType]string{
	domain.LinkBuildsOn:    "BUILDS_ON",
	domain.LinkContradicts: "CONTRADICTS",
	domain.LinkRelated:     "RELATED",
}

func linkTypeOf(rel string) domain.LinkType { return domain.LinkType(strings.ToLower(rel)) }

// NewHypothesis is a registration: who, where, the claim, and where it sits in the graph.
type NewHypothesis struct {
	// AgentID is the agent whose hypothesis it is, empty when no agent owns it, and Actor whose
	// credential registers it.
	AgentID              string
	Actor                domain.Actor
	PlatformExperimentID string
	Text                 string
	Links                []domain.LinkSpec
	// DistinctFrom names hypotheses the caller has read and judged a different claim, so they no
	// longer hold the registration back (see untoldSimilar).
	DistinctFrom []string
	// Stamp is the watch stream position of this write and its check for an earlier aborted one
	// (see db.EventsStore.Emit).
	Stamp db.Stamp
}

// Register creates a hypothesis and its outgoing links in one transaction, or returns the existing
// hypothesis with equivalent normalized text in the same platform experiment (alreadyExisted=true)
// without applying the supplied links. links are the hypothesis's outgoing links either way.
// announce is true when the caller emits hypothesis.new: the hypothesis is new, or its earlier
// registration's event was never sent (see restamp). It creates nothing and returns a
// *SimilarError while other hypotheses read close to the new one and the caller has not told it
// apart from them.
func (s *Store) Register(ctx context.Context, n NewHypothesis) (h *domain.Hypothesis, links []domain.Link, alreadyExisted, announce bool, err error) {
	normalized := domain.NormalizeHypothesisText(n.Text)
	if normalized == "" {
		return nil, nil, false, false, fmt.Errorf("graphdb.Register: text is empty")
	}
	if err := validateLinks(n.PlatformExperimentID, n.Links, true); err != nil {
		return nil, nil, false, false, err
	}
	if err := s.ensureObjective(ctx, n.PlatformExperimentID); err != nil {
		return nil, nil, false, false, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, nil, false, false, fmt.Errorf("graphdb.Register: %w", err)
	}
	at := now()

	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		if err := lockExperiment(ctx, tx, n.PlatformExperimentID); err != nil {
			return nil, err
		}
		existing, err := findByNormalizedText(ctx, tx, n.PlatformExperimentID, normalized)
		if err != nil || existing != nil {
			if err == nil {
				announce, err = restamp(ctx, tx, n.Stamp, "MATCH (h:Hypothesis {id: $id})", "h.created_xid", map[string]any{"id": existing.ID})
			}
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
  actor_kind: $actor_kind, actor_id: $actor_id, text: $text, normalized_text: $normalized, status: $status, created_at: $at, created_xid: $position})
RETURN h`, map[string]any{
			"id": id.String(), "pe": n.PlatformExperimentID, "agent_id": n.AgentID,
			"actor_kind": string(n.Actor.Kind), "actor_id": n.Actor.ID, "text": n.Text, "normalized": normalized, "status": string(domain.HypothesisOpen), "at": at,
			"position": n.Stamp.Position,
		})
		if err != nil {
			return nil, err
		}
		for i, spec := range n.Links {
			if _, err := createLink(ctx, tx, id.String(), n.PlatformExperimentID, spec, i, n.AgentID, n.Actor, at, n.Stamp.Position); err != nil {
				return nil, err
			}
		}
		announce = true
		return hypothesisFromNode(records[0].Values[0].(neo4j.Node))
	})
	if err != nil {
		return nil, nil, false, false, wrap("graphdb.Register", err)
	}
	h = out.(*domain.Hypothesis)
	alreadyExisted = h.ID != id.String()
	links, err = s.outLinks(ctx, h.ID)
	if err != nil {
		return nil, nil, false, false, err
	}
	return h, links, alreadyExisted, announce, nil
}

// LinkResult is the outcome of AddLink. Created is false when the link already existed, which is a
// no-op; CreatedAt is then zero. Announce is true when the caller emits hypothesis.link: the link
// is new, or its earlier addition's event was never sent (see restamp).
type LinkResult struct {
	From      *domain.Hypothesis
	Created   bool
	Announce  bool
	CreatedAt time.Time
	Out       []domain.Link
}

// AddLink adds one outgoing link from an existing hypothesis at watch stream position
// stamp.Position. Only its registering agent may, or anyone when it was submitted by a human.
// Links are append-only. agentID is the agent whose link it is, empty when no agent owns it, and
// actor whose credential adds it.
func (s *Store) AddLink(ctx context.Context, fromID, agentID string, actor domain.Actor, spec domain.LinkSpec, stamp db.Stamp) (*LinkResult, error) {
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
		if err := lockExperiment(ctx, tx, from.PlatformExperimentID); err != nil {
			return nil, err
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
		created, err := createLink(ctx, tx, fromID, from.PlatformExperimentID, spec, 0, agentID, actor, at, stamp.Position)
		if err != nil {
			return nil, err
		}
		result := &LinkResult{From: from, Created: created, Announce: created}
		if created {
			result.CreatedAt = at
			return result, nil
		}
		// A link given at registration is announced by hypothesis.new, so only a later one is re-sent.
		result.Announce, err = restamp(ctx, tx, stamp,
			linkMatch(from.PlatformExperimentID, spec)+" MATCH (h)-[r:"+relTypes[spec.Type]+"]->(t) WHERE r.created_at <> h.created_at",
			"r.created_xid", map[string]any{"from": fromID, "to": spec.To})
		return result, err
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

// UpdateStatus sets a hypothesis's status on behalf of callerAgentID, at watch stream position
// stamp.Position: the owning agent's verdict on its own claim, or anyone's on one no agent owns.
// actor is whose credential gives the verdict, kept with the status it set. announce is true when the caller emits hypothesis.status: the status changed, or it already
// held that value and the event of the write that set it was never sent (see restamp).
func (s *Store) UpdateStatus(ctx context.Context, id, callerAgentID string, actor domain.Actor, status domain.HypothesisStatus, stamp db.Stamp) (h *domain.Hypothesis, announce bool, err error) {
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
			announce, err = restamp(ctx, tx, stamp, "MATCH (h:Hypothesis {id: $id})", "h.status_changed_xid", map[string]any{"id": id})
			return h, err
		}
		if err := lockExperiment(ctx, tx, h.PlatformExperimentID); err != nil {
			return nil, err
		}
		records, err := collect(ctx, tx, `MATCH (h:Hypothesis {id: $id}) SET h.status = $status, h.status_changed_at = $at, h.status_changed_xid = $position,
  h.status_actor_kind = $actor_kind, h.status_actor_id = $actor_id RETURN h`,
			map[string]any{"id": id, "status": string(status), "at": at, "position": stamp.Position,
				"actor_kind": string(actor.Kind), "actor_id": actor.ID})
		if err != nil {
			return nil, err
		}
		announce = true
		return hypothesisFromNode(records[0].Values[0].(neo4j.Node))
	})
	if err != nil {
		return nil, false, wrap("graphdb.UpdateStatus", err)
	}
	return out.(*domain.Hypothesis), announce, nil
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
		outgoing, err := linksTx(ctx, tx, `MATCH (:Hypothesis {id: $id})-[r]->(t) RETURN type(r), t, r ORDER BY `+linkOrder+` LIMIT $limit`, id)
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
	Limit      int
	Offset     int
}

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
WITH h ORDER BY h.created_at DESC, h.id DESC SKIP $offset LIMIT $limit`+itemTail, params)
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

// ImportLegacy copies hypotheses from the retired Postgres table into the graph, each building on
// its experiment's objective. Rows already present (by id) are left as they are, so a rerun after
// an interrupted migration is harmless. They come from before the credential behind a write was
// kept, so each is stored with no actor and the author it stated, which is h.Actor.Name.
func (s *Store) ImportLegacy(ctx context.Context, rows []*domain.Hypothesis) error {
	params := make([]map[string]any, 0, len(rows))
	for _, h := range rows {
		var statusChangedAt any
		if h.StatusChangedAt != nil {
			statusChangedAt = h.StatusChangedAt.UTC()
		}
		params = append(params, map[string]any{
			"id": h.ID, "pe": h.PlatformExperimentID, "agent_id": h.AgentID,
			"author": h.Actor.Name, "text": h.Text, "normalized": domain.NormalizeHypothesisText(h.Text),
			"status": string(h.Status), "created_at": h.CreatedAt.UTC(), "status_changed_at": statusChangedAt,
		})
	}
	_, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return collect(ctx, tx, `UNWIND $rows AS row
MERGE (o:Objective {id: row.pe})
MERGE (h:Hypothesis {id: row.id})
ON CREATE SET h.platform_experiment_id = row.pe, h.agent_id = row.agent_id,
  h.author = row.author, h.text = row.text, h.normalized_text = row.normalized, h.status = row.status,
  h.created_at = row.created_at, h.status_changed_at = row.status_changed_at, h.created_xid = 0,
  h.status_changed_xid = CASE WHEN row.status_changed_at IS NULL THEN null ELSE 0 END
MERGE (h)-[r:BUILDS_ON]->(o)
ON CREATE SET r.agent_id = row.agent_id, r.author = row.author, r.created_at = row.created_at, r.created_xid = 0`, map[string]any{"rows": params})
	})
	if err != nil {
		return fmt.Errorf("graphdb.ImportLegacy: %w", err)
	}
	return nil
}

// CreditUsers gives hypotheses and links written before actors were kept the signed-in user the
// retired human_writes table holds for them (see db.MoveHumanWrites). A hypothesis takes its
// links made at registration with it. Only a record with no actor is touched, so running it
// again changes nothing.
func (s *Store) CreditUsers(ctx context.Context, writes []db.HumanWrite) error {
	var hypotheses []map[string]any
	links := map[string][]map[string]any{}
	for _, w := range writes {
		switch w.Kind {
		case "hypothesis":
			hypotheses = append(hypotheses, map[string]any{"id": w.Subject, "user": w.UserID})
		case "link":
			parts := strings.Split(w.Subject, " ")
			if len(parts) != 3 {
				return fmt.Errorf("graphdb.CreditUsers: link %q is not \"<from> <type> <to>\"", w.Subject)
			}
			rel, ok := relTypes[domain.LinkType(parts[1])]
			if !ok {
				return fmt.Errorf("graphdb.CreditUsers: link %q has an unknown type", w.Subject)
			}
			links[rel] = append(links[rel], map[string]any{"from": parts[0], "to": parts[2], "user": w.UserID})
		default:
			return fmt.Errorf("graphdb.CreditUsers: write of unknown kind %q", w.Kind)
		}
	}
	_, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		if _, err := collect(ctx, tx, `UNWIND $rows AS row
MATCH (h:Hypothesis {id: row.id}) WHERE h.actor_kind IS NULL
SET h.actor_kind = 'user', h.actor_id = row.user
WITH h, row
MATCH (h)-[r]->() WHERE r.actor_kind IS NULL AND r.created_at = h.created_at
SET r.actor_kind = 'user', r.actor_id = row.user`, map[string]any{"rows": hypotheses}); err != nil {
			return nil, err
		}
		for rel, rows := range links {
			if _, err := collect(ctx, tx, `UNWIND $rows AS row
MATCH (:Hypothesis {id: row.from})-[r:`+rel+`]->(t {id: row.to}) WHERE r.actor_kind IS NULL
SET r.actor_kind = 'user', r.actor_id = row.user`, map[string]any{"rows": rows}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		return fmt.Errorf("graphdb.CreditUsers: %w", err)
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

// lockExperiment makes tx the only writer of one platform experiment's graph until it ends. Every
// write transaction modifies the objective first, so Memgraph aborts an overlapping one with a
// conflict that ExecuteWrite retries once the first has committed. The checks that read before
// writing (dedup, similarity, cycles) rely on this: two writes that touch no common hypothesis
// would otherwise both pass them.
func lockExperiment(ctx context.Context, tx neo4j.ManagedTransaction, platformExperimentID string) error {
	records, err := collect(ctx, tx, `MATCH (o:Objective {id: $pe}) SET o.revision = coalesce(o.revision, 0) + 1 RETURN o.id`,
		map[string]any{"pe": platformExperimentID})
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return fmt.Errorf("graphdb: platform experiment %s has no objective", platformExperimentID)
	}
	return nil
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
// relTypes, after validateLinks. ordinal is the link's place among those one write states, which
// share a created_at: it keeps them in the order stated (linkOrder).
func createLink(ctx context.Context, tx neo4j.ManagedTransaction, fromID, platformExperimentID string, spec domain.LinkSpec, ordinal int, agentID string, actor domain.Actor, at time.Time, position int64) (bool, error) {
	records, err := collect(ctx, tx, linkMatch(platformExperimentID, spec)+`
MERGE (h)-[r:`+relTypes[spec.Type]+`]->(t)
ON CREATE SET r.agent_id = $agent_id, r.actor_kind = $actor_kind, r.actor_id = $actor_id, r.created_at = $at, r.created_xid = $position, r.ordinal = $ordinal
RETURN r.created_at = $at`, map[string]any{"from": fromID, "to": spec.To, "agent_id": agentID,
		"actor_kind": string(actor.Kind), "actor_id": actor.ID, "at": at, "position": position, "ordinal": ordinal})
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
			CreatedAt:    d.time(rel.Props["created_at"]),
		}
		link.Actor = storedActor(&d, rel.Props, "actor_kind", "actor_id", link.AgentID)
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

// linkMatch binds h to the hypothesis $from and t to the link target $to.
func linkMatch(platformExperimentID string, spec domain.LinkSpec) string {
	if spec.To == platformExperimentID {
		return "MATCH (h:Hypothesis {id: $from}), (t:Objective {id: $to})"
	}
	return "MATCH (h:Hypothesis {id: $from}), (t:Hypothesis {id: $to})"
}

// restamp re-sends an event whose write committed here while its Postgres transaction aborted:
// when the position at prop, read through match, is an aborted transaction's, it records
// stamp.Position there instead and reports true, so the caller emits the event again from a
// transaction that has to commit for the write to count. A position the stamping transaction is
// still running on is left to that transaction's own caller, which retries if it aborts.
func restamp(ctx context.Context, tx neo4j.ManagedTransaction, stamp db.Stamp, match, prop string, params map[string]any) (bool, error) {
	records, err := collect(ctx, tx, match+" RETURN "+prop, params)
	if err != nil || len(records) == 0 {
		return false, err
	}
	position, _ := records[0].Values[0].(int64)
	if position == 0 {
		return false, nil
	}
	aborted, err := stamp.Aborted(position)
	if err != nil || !aborted {
		return false, err
	}
	params["position"] = stamp.Position
	if _, err := collect(ctx, tx, match+" SET "+prop+" = $position", params); err != nil {
		return false, err
	}
	return true, nil
}

func hypothesisFromNode(n neo4j.Node) (*domain.Hypothesis, error) {
	p := n.Props
	var d decoder
	h := &domain.Hypothesis{
		ID:                   d.str(p["id"]),
		AgentID:              d.str(p["agent_id"]),
		PlatformExperimentID: d.str(p["platform_experiment_id"]),
		Text:                 d.str(p["text"]),
		Status:               domain.HypothesisStatus(d.str(p["status"])),
		CreatedAt:            d.time(p["created_at"]),
	}
	h.Actor = storedActor(&d, p, "actor_kind", "actor_id", h.AgentID)
	if v, ok := p["status_changed_at"]; ok && v != nil {
		changed := d.time(v)
		h.StatusChangedAt = &changed
		// A verdict from before its giver was kept names nobody but the agent it had to be given as.
		by := domain.StoredActor(domain.ActorUnverified, "", "", h.AgentID)
		if kind, ok := p["status_actor_kind"]; ok && kind != nil {
			by = domain.StoredActor(domain.ActorKind(d.str(kind)), d.str(p["status_actor_id"]), "", h.AgentID)
		}
		h.StatusActor = &by
	}
	if d.err != nil {
		return nil, fmt.Errorf("hypothesis %v: %w", p["id"], d.err)
	}
	return h, nil
}

// storedActor reads the actor kept under two properties of a node or relationship. One written
// before the credential behind a write was kept has neither and is unverified, named by the
// author it stated or else the agent it named.
func storedActor(d *decoder, props map[string]any, kindProp, idProp, agentID string) domain.Actor {
	kind, ok := props[kindProp]
	if !ok || kind == nil {
		author := ""
		if stated, ok := props["author"]; ok && stated != nil {
			author = d.str(stated)
		}
		return domain.StoredActor(domain.ActorUnverified, "", author, agentID)
	}
	return domain.StoredActor(domain.ActorKind(d.str(kind)), d.str(props[idProp]), "", agentID)
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
