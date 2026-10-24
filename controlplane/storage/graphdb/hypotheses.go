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

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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
}

// Register creates a hypothesis and its outgoing links in one transaction, or returns the existing
// hypothesis with equivalent normalized text in the same platform experiment (alreadyExisted=true)
// without applying the supplied links. links are the hypothesis's outgoing links either way.
// It creates nothing and returns a
// *SimilarError while other hypotheses read close to the new one and the caller has not told it
// apart from them. A signed-in person is never held back: their hypothesis is created and the
// ones it reads close to come back as similar.
func (s *Store) Register(ctx context.Context, n NewHypothesis) (h *domain.Hypothesis, links []domain.Link, alreadyExisted bool, similar []domain.SimilarHypothesis, err error) {
	normalized := domain.NormalizeHypothesisText(n.Text)
	if normalized == "" {
		return nil, nil, false, nil, fmt.Errorf("graphdb.Register: text is empty")
	}
	if err := validateLinks(n.PlatformExperimentID, n.Links, true); err != nil {
		return nil, nil, false, nil, err
	}
	if err := s.ensureObjective(ctx, n.PlatformExperimentID); err != nil {
		return nil, nil, false, nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, nil, false, nil, fmt.Errorf("graphdb.Register: %w", err)
	}
	at := now()

	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		similar = nil
		if err := lockExperiment(ctx, tx, n.PlatformExperimentID); err != nil {
			return nil, err
		}
		existing, err := findByNormalizedText(ctx, tx, n.PlatformExperimentID, normalized)
		if err != nil || existing != nil {
			return existing, err
		}
		if err := checkTargets(ctx, tx, n.PlatformExperimentID, "", n.Links); err != nil {
			return nil, err
		}
		similar, err = untoldSimilar(ctx, tx, n)
		if err != nil {
			return nil, err
		}
		if len(similar) > 0 && n.Actor.Kind != domain.ActorUser {
			return nil, &SimilarError{Similar: similar}
		}
		records, err := collect(ctx, tx, `CREATE (h:Hypothesis {id: $id, platform_experiment_id: $pe, agent_id: $agent_id,
  actor_kind: $actor_kind, actor_id: $actor_id, text: $text, normalized_text: $normalized, status: $status, created_at: $at})
RETURN h`, map[string]any{
			"id": id.String(), "pe": n.PlatformExperimentID, "agent_id": n.AgentID,
			"actor_kind": string(n.Actor.Kind), "actor_id": n.Actor.ID, "text": n.Text, "normalized": normalized, "status": string(domain.HypothesisOpen), "at": at,
		})
		if err != nil {
			return nil, err
		}
		for i, spec := range n.Links {
			if _, err := createLink(ctx, tx, id.String(), n.PlatformExperimentID, spec, i, n.AgentID, n.Actor, at); err != nil {
				return nil, err
			}
		}
		return hypothesisFromNode(records[0].Values[0].(neo4j.Node))
	})
	if err != nil {
		return nil, nil, false, nil, wrap("graphdb.Register", err)
	}
	h = out.(*domain.Hypothesis)
	alreadyExisted = h.ID != id.String()
	links, err = s.outLinks(ctx, h.ID)
	if err != nil {
		return nil, nil, false, nil, err
	}
	return h, links, alreadyExisted, similar, nil
}

// LinkResult is the outcome of AddLink. Created is false when the link already existed, which is a
// no-op; CreatedAt is then zero. Later is true for a link added after its hypothesis was
// registered, which hypothesis.link announces: one given at registration rides on hypothesis.new.
type LinkResult struct {
	From      *domain.Hypothesis
	Created   bool
	Later     bool
	CreatedAt time.Time
	Out       []domain.Link
}

// AddLink adds one outgoing link from an existing hypothesis that is neither merged nor retired.
// Who may is the caller's to decide. agentID is the agent whose link it is, empty when no agent
// owns it, and actor whose credential adds it.
func (s *Store) AddLink(ctx context.Context, fromID, agentID string, actor domain.Actor, spec domain.LinkSpec) (*LinkResult, error) {
	at := now()
	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		from, err := lockedHypothesis(ctx, tx, fromID)
		if err != nil {
			return nil, err
		}
		if err := validateLinks(from.PlatformExperimentID, []domain.LinkSpec{spec}, false); err != nil {
			return nil, err
		}
		if from.Curated() {
			return nil, &CuratedError{Hypothesis: from, Refused: "takes no link"}
		}
		if err := checkTargets(ctx, tx, from.PlatformExperimentID, fromID, []domain.LinkSpec{spec}); err != nil {
			return nil, err
		}
		if spec.Type == domain.LinkBuildsOn && spec.To != from.PlatformExperimentID {
			if err := checkNoCycle(ctx, tx, fromID, spec.To); err != nil {
				return nil, err
			}
		}
		created, err := createLink(ctx, tx, fromID, from.PlatformExperimentID, spec, 0, agentID, actor, at)
		if err != nil {
			return nil, err
		}
		result := &LinkResult{From: from, Created: created, Later: created}
		if created {
			result.CreatedAt = at
			return result, nil
		}
		records, err := collect(ctx, tx, linkMatch(from.PlatformExperimentID, spec)+" MATCH (h)-[r:"+relTypes[spec.Type]+"]->(t) RETURN r.created_at <> h.created_at",
			map[string]any{"from": fromID, "to": spec.To})
		if err != nil {
			return nil, err
		}
		result.Later = len(records) > 0 && records[0].Values[0] == true
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

// UpdateStatus sets a hypothesis's status on behalf of callerAgentID: the owning agent's verdict
// on its own claim, or anyone's on one no agent owns. actor is whose credential gives the verdict,
// kept with the status it set. changed is false when it already held status.
func (s *Store) UpdateStatus(ctx context.Context, id, callerAgentID string, actor domain.Actor, status domain.HypothesisStatus) (h *domain.Hypothesis, changed bool, err error) {
	at := now()
	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		changed = false
		h, err := lockedHypothesis(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if h.AgentID != "" && h.AgentID != callerAgentID {
			return nil, fmt.Errorf("%w: hypothesis %s belongs to agent %s", ErrNotOwner, id, h.AgentID)
		}
		if h.Curated() {
			return nil, &CuratedError{Hypothesis: h, Refused: "takes no verdict"}
		}
		if h.Status == status {
			return h, nil
		}
		records, err := collect(ctx, tx, `MATCH (h:Hypothesis {id: $id}) SET h.status = $status, h.status_changed_at = $at,
  h.status_actor_kind = $actor_kind, h.status_actor_id = $actor_id RETURN h`,
			map[string]any{"id": id, "status": string(status), "at": at,
				"actor_kind": string(actor.Kind), "actor_id": actor.ID})
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

// EditText replaces a hypothesis's text while no other hypothesis links to it. Who may edit is
// the caller's to decide. It is refused with a *LinkedError once one does, and with
// ErrDuplicateText when another hypothesis of the platform experiment states the new text.
func (s *Store) EditText(ctx context.Context, id, text string) (*domain.Hypothesis, error) {
	normalized := domain.NormalizeHypothesisText(text)
	if normalized == "" {
		return nil, fmt.Errorf("graphdb.EditText: text is empty")
	}
	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		h, err := unlinked(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		existing, err := findByNormalizedText(ctx, tx, h.PlatformExperimentID, normalized)
		if err != nil {
			return nil, err
		}
		if existing != nil && existing.ID != id {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateText, existing.ID)
		}
		return oneHypothesis(ctx, tx, `MATCH (h:Hypothesis {id: $id}) SET h.text = $text, h.normalized_text = $normalized RETURN h`,
			map[string]any{"id": id, "text": text, "normalized": normalized})
	})
	if err != nil {
		return nil, wrap("graphdb.EditText", err)
	}
	return out.(*domain.Hypothesis), nil
}

// Remove deletes a hypothesis and its outgoing links while no other hypothesis links to it, and
// refuses with a *LinkedError once one does. Who may remove is the caller's to decide.
func (s *Store) Remove(ctx context.Context, id string) error {
	_, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		if _, err := unlinked(ctx, tx, id); err != nil {
			return nil, err
		}
		return collect(ctx, tx, `MATCH (h:Hypothesis {id: $id}) DETACH DELETE h`, map[string]any{"id": id})
	})
	if err != nil {
		return wrap("graphdb.Remove", err)
	}
	return nil
}

// unlinked returns the hypothesis id within tx, holding its platform experiment's lock, while
// nothing links to it.
func unlinked(ctx context.Context, tx neo4j.ManagedTransaction, id string) (*domain.Hypothesis, error) {
	h, err := getHypothesis(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := lockExperiment(ctx, tx, h.PlatformExperimentID); err != nil {
		return nil, err
	}
	in, err := linksTx(ctx, tx, incomingLinks, id)
	if err != nil {
		return nil, err
	}
	if len(in) > 0 {
		return nil, &LinkedError{In: in}
	}
	return h, nil
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

// checkTargets verifies every hypothesis a link points at exists, belongs to platformExperimentID,
// is neither merged nor retired and is not fromID itself.
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
	records, err := collect(ctx, tx, `UNWIND $ids AS id MATCH (t:Hypothesis {id: id}) RETURN t`, map[string]any{"ids": ids})
	if err != nil {
		return err
	}
	found := make(map[string]*domain.Hypothesis, len(records))
	for _, r := range records {
		h, err := hypothesisFromNode(r.Values[0].(neo4j.Node))
		if err != nil {
			return err
		}
		found[h.ID] = h
	}
	for _, id := range ids {
		h, ok := found[id]
		switch {
		case !ok:
			return fmt.Errorf("%w: no hypothesis %q; a link targets an existing hypothesis id in platform experiment %s, or %s itself for the objective", ErrInvalidLink, id, platformExperimentID, platformExperimentID)
		case h.PlatformExperimentID != platformExperimentID:
			return fmt.Errorf("%w: hypothesis %s belongs to platform experiment %s; links stay within %s", ErrInvalidLink, id, h.PlatformExperimentID, platformExperimentID)
		case h.Status == domain.HypothesisMerged:
			return fmt.Errorf("%w: hypothesis %s was merged into %s; link to %s instead", ErrInvalidLink, id, h.MergedInto, h.MergedInto)
		case h.Status == domain.HypothesisRetired:
			return fmt.Errorf("%w: hypothesis %s was retired (%s) and takes no link", ErrInvalidLink, id, h.StatusReason)
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
func createLink(ctx context.Context, tx neo4j.ManagedTransaction, fromID, platformExperimentID string, spec domain.LinkSpec, ordinal int, agentID string, actor domain.Actor, at time.Time) (bool, error) {
	records, err := collect(ctx, tx, linkMatch(platformExperimentID, spec)+`
MERGE (h)-[r:`+relTypes[spec.Type]+`]->(t)
ON CREATE SET r.agent_id = $agent_id, r.actor_kind = $actor_kind, r.actor_id = $actor_id, r.created_at = $at, r.ordinal = $ordinal
RETURN r.created_at = $at`, map[string]any{"from": fromID, "to": spec.To, "agent_id": agentID,
		"actor_kind": string(actor.Kind), "actor_id": actor.ID, "at": at, "ordinal": ordinal})
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

// linkMatch binds h to the hypothesis $from and t to the link target $to.
func linkMatch(platformExperimentID string, spec domain.LinkSpec) string {
	if spec.To == platformExperimentID {
		return "MATCH (h:Hypothesis {id: $from}), (t:Objective {id: $to})"
	}
	return "MATCH (h:Hypothesis {id: $from}), (t:Hypothesis {id: $to})"
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
	if v, ok := p["merged_into"]; ok && v != nil {
		h.MergedInto = d.str(v)
	}
	if v, ok := p["status_reason"]; ok && v != nil {
		h.StatusReason = d.str(v)
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
	for _, sentinel := range []error{ErrNotFound, ErrNotOwner, ErrInvalidLink, ErrSimilar, ErrDuplicateText, ErrNotOpen, ErrCurated} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
