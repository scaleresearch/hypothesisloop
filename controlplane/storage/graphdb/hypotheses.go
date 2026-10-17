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
	// Stamp is the watch stream position of this write and its check for an earlier aborted one
	// (see db.EventsStore.Emit).
	Stamp domain.Stamp
}

// Register creates a hypothesis and its outgoing links in one transaction, or returns the existing
// hypothesis with equivalent normalized text in the same platform experiment (alreadyExisted=true)
// without applying the supplied links. links are the hypothesis's outgoing links either way.
// announce is true when the caller emits hypothesis.new: the hypothesis is new, or its earlier
// registration's event was never sent (see restamp). It creates nothing and returns a
// *SimilarError while other hypotheses read close to the new one and the caller has not told it
// apart from them. A signed-in person is never held back: their hypothesis is created and the
// ones it reads close to come back as similar.
func (s *Store) Register(ctx context.Context, n NewHypothesis) (h *domain.Hypothesis, links []domain.Link, alreadyExisted, announce bool, similar []domain.SimilarHypothesis, err error) {
	normalized := domain.NormalizeHypothesisText(n.Text)
	if normalized == "" {
		return nil, nil, false, false, nil, fmt.Errorf("graphdb.Register: text is empty")
	}
	if err := validateLinks(n.PlatformExperimentID, n.Links, true); err != nil {
		return nil, nil, false, false, nil, err
	}
	if err := s.ensureObjective(ctx, n.PlatformExperimentID); err != nil {
		return nil, nil, false, false, nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, nil, false, false, nil, fmt.Errorf("graphdb.Register: %w", err)
	}
	at := now()

	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		similar = nil
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
		similar, err = untoldSimilar(ctx, tx, n)
		if err != nil {
			return nil, err
		}
		if len(similar) > 0 && n.Actor.Kind != domain.ActorUser {
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
		return nil, nil, false, false, nil, wrap("graphdb.Register", err)
	}
	h = out.(*domain.Hypothesis)
	alreadyExisted = h.ID != id.String()
	links, err = s.outLinks(ctx, h.ID)
	if err != nil {
		return nil, nil, false, false, nil, err
	}
	return h, links, alreadyExisted, announce, similar, nil
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
func (s *Store) AddLink(ctx context.Context, fromID, agentID string, actor domain.Actor, spec domain.LinkSpec, stamp domain.Stamp) (*LinkResult, error) {
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
func (s *Store) UpdateStatus(ctx context.Context, id, callerAgentID string, actor domain.Actor, status domain.HypothesisStatus, stamp domain.Stamp) (h *domain.Hypothesis, announce bool, err error) {
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
func restamp(ctx context.Context, tx neo4j.ManagedTransaction, stamp domain.Stamp, match, prop string, params map[string]any) (bool, error) {
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
	for _, sentinel := range []error{ErrNotFound, ErrNotOwner, ErrInvalidLink, ErrSimilar, ErrDuplicateText} {
		if errors.Is(err, sentinel) {
			return err
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
