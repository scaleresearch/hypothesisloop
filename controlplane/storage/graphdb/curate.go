package graphdb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

var (
	// ErrNotOpen is a merge or retirement of a hypothesis that is no longer open: a settled one is
	// evidence, and a merged or retired one was curated already.
	ErrNotOpen = errors.New("only an open hypothesis can be merged or retired")
	// ErrCurated is a link from, an unlink from or a verdict on a merged or retired hypothesis.
	// The error is a *CuratedError naming it.
	ErrCurated = errors.New("hypothesis was merged or retired")
)

// CuratedError refuses a write to a merged or retired hypothesis, what for in Refused.
type CuratedError struct {
	Hypothesis *domain.Hypothesis
	Refused    string
}

func (e *CuratedError) Error() string {
	h := e.Hypothesis
	if h.Status == domain.HypothesisMerged {
		return fmt.Sprintf("hypothesis %s was merged into %s and %s: write to %s instead", h.ID, h.MergedInto, e.Refused, h.MergedInto)
	}
	return fmt.Sprintf("hypothesis %s was retired (%s) and %s", h.ID, h.StatusReason, e.Refused)
}

func (e *CuratedError) Unwrap() error { return ErrCurated }

// Merge records that dupID states the claim of intoID: every link into dupID is moved to intoID
// with its provenance, and dupID becomes merged with merged_into = intoID, as does every
// hypothesis merged into dupID before, so merged_into always names one that is not merged. A
// link from intoID itself is dropped, as is one intoID already has from the same hypothesis.
// dupID's own links stay as its history. Both are of one platform experiment, dupID is open and intoID is neither
// merged nor retired. Merging again into the same hypothesis changes nothing (changed false).
func (s *Store) Merge(ctx context.Context, dupID, intoID string, actor domain.Actor, reason string) (h *domain.Hypothesis, changed bool, err error) {
	at := now()
	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		changed = false
		dup, err := lockedHypothesis(ctx, tx, dupID)
		if err != nil {
			return nil, err
		}
		if dup.Status == domain.HypothesisMerged && dup.MergedInto == intoID {
			return dup, nil
		}
		if err := mergeable(ctx, tx, dup, intoID); err != nil {
			return nil, err
		}
		for _, rel := range relTypes {
			if _, err := collect(ctx, tx, `MATCH (x:Hypothesis)-[r:`+rel+`]->(:Hypothesis {id: $dup}), (i:Hypothesis {id: $into})
WHERE x.id <> $into AND NOT exists((x)-[:`+rel+`]->(i))
CREATE (x)-[n:`+rel+`]->(i) SET n = properties(r)`, map[string]any{"dup": dupID, "into": intoID}); err != nil {
				return nil, err
			}
		}
		if _, err := collect(ctx, tx, `MATCH (:Hypothesis)-[r]->(:Hypothesis {id: $dup}) DELETE r`, map[string]any{"dup": dupID}); err != nil {
			return nil, err
		}
		if _, err := collect(ctx, tx, `MATCH (m:Hypothesis {platform_experiment_id: $pe, merged_into: $dup}) SET m.merged_into = $into`,
			map[string]any{"pe": dup.PlatformExperimentID, "dup": dupID, "into": intoID}); err != nil {
			return nil, err
		}
		changed = true
		return oneHypothesis(ctx, tx, `MATCH (h:Hypothesis {id: $id}) SET h.status = $status, h.merged_into = $into, h.status_reason = $reason,
  h.status_changed_at = $at, h.status_actor_kind = $actor_kind, h.status_actor_id = $actor_id RETURN h`,
			map[string]any{"id": dupID, "status": string(domain.HypothesisMerged), "into": intoID, "reason": reason, "at": at,
				"actor_kind": string(actor.Kind), "actor_id": actor.ID})
	})
	if err != nil {
		return nil, false, wrap("graphdb.Merge", err)
	}
	return out.(*domain.Hypothesis), changed, nil
}

// mergeable refuses a merge of dup into intoID the graph cannot take: a target that is dup
// itself, missing, of another platform experiment or curated; a dup that is not open; an intoID
// that builds on dup alone, which would be left on nothing; and a builds_on moved to intoID that
// would close a cycle.
func mergeable(ctx context.Context, tx neo4j.ManagedTransaction, dup *domain.Hypothesis, intoID string) error {
	if intoID == dup.ID {
		return fmt.Errorf("%w: a hypothesis cannot be merged into itself", ErrInvalidLink)
	}
	if dup.Status != domain.HypothesisOpen {
		return fmt.Errorf("%w: hypothesis %s is %s", ErrNotOpen, dup.ID, dup.Status)
	}
	if err := checkTargets(ctx, tx, dup.PlatformExperimentID, dup.ID, []domain.LinkSpec{{Type: domain.LinkRelated, To: intoID}}); err != nil {
		return err
	}
	records, err := collect(ctx, tx, `MATCH (i:Hypothesis {id: $into})-[:BUILDS_ON]->(t) RETURN collect(t.id)`, map[string]any{"into": intoID})
	if err != nil {
		return err
	}
	if parents := records[0].Values[0].([]any); len(parents) == 1 && parents[0] == dup.ID {
		return fmt.Errorf("%w: %s builds on %s alone and would be left on nothing: link it elsewhere first (builds_on)", ErrInvalidLink, intoID, dup.ID)
	}
	records, err = collect(ctx, tx, `MATCH (x:Hypothesis)-[:BUILDS_ON]->(:Hypothesis {id: $dup}), (i:Hypothesis {id: $into})
WHERE x.id <> $into MATCH (i)-[:BUILDS_ON *BFS]->(x) RETURN DISTINCT x.id ORDER BY x.id`, map[string]any{"dup": dup.ID, "into": intoID})
	if err != nil {
		return err
	}
	if len(records) > 0 {
		var d decoder
		ids := make([]string, len(records))
		for i, r := range records {
			ids[i] = d.str(r.Values[0])
		}
		if d.err != nil {
			return d.err
		}
		return fmt.Errorf("%w: %s builds on %s and %s already builds on it, so moving its builds_on to %s would make a cycle: unlink one first",
			ErrInvalidLink, strings.Join(ids, ", "), dup.ID, intoID, intoID)
	}
	return nil
}

// Retire takes an open hypothesis off the frontier with reason. Retiring a retired one changes
// nothing (changed false).
func (s *Store) Retire(ctx context.Context, id string, actor domain.Actor, reason string) (h *domain.Hypothesis, changed bool, err error) {
	at := now()
	out, err := s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		changed = false
		h, err := lockedHypothesis(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if h.Status == domain.HypothesisRetired {
			return h, nil
		}
		if h.Status != domain.HypothesisOpen {
			return nil, fmt.Errorf("%w: hypothesis %s is %s", ErrNotOpen, id, h.Status)
		}
		changed = true
		return oneHypothesis(ctx, tx, `MATCH (h:Hypothesis {id: $id}) SET h.status = $status, h.status_reason = $reason,
  h.status_changed_at = $at, h.status_actor_kind = $actor_kind, h.status_actor_id = $actor_id RETURN h`,
			map[string]any{"id": id, "status": string(domain.HypothesisRetired), "reason": reason, "at": at,
				"actor_kind": string(actor.Kind), "actor_id": actor.ID})
	})
	if err != nil {
		return nil, false, wrap("graphdb.Retire", err)
	}
	return out.(*domain.Hypothesis), changed, nil
}

// Unlink removes one outgoing link of fromID, reporting whether there was one: removing an absent
// link changes nothing. It refuses to remove the last builds_on of a hypothesis, so a relink adds
// the new link first, and any link of a merged or retired one, whose links are its history. out
// is fromID's outgoing links after.
func (s *Store) Unlink(ctx context.Context, fromID string, spec domain.LinkSpec) (removed bool, out []domain.Link, err error) {
	_, err = s.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		removed = false
		from, err := lockedHypothesis(ctx, tx, fromID)
		if err != nil {
			return nil, err
		}
		if err := validateLinks(from.PlatformExperimentID, []domain.LinkSpec{spec}, false); err != nil {
			return nil, err
		}
		if from.Curated() {
			return nil, &CuratedError{Hypothesis: from, Refused: "its links are its history"}
		}
		params := map[string]any{"from": fromID, "to": spec.To}
		records, err := collect(ctx, tx, linkMatch(from.PlatformExperimentID, spec)+` MATCH (h)-[r:`+relTypes[spec.Type]+`]->(t)
OPTIONAL MATCH (h)-[b:BUILDS_ON]->()
RETURN count(DISTINCT r), count(DISTINCT b)`, params)
		if err != nil {
			return nil, err
		}
		var d decoder
		links, buildsOn := d.int(records[0].Values[0]), d.int(records[0].Values[1])
		if d.err != nil || links == 0 {
			return nil, d.err
		}
		if spec.Type == domain.LinkBuildsOn && buildsOn == 1 {
			return nil, fmt.Errorf("%w: builds_on %s is the last builds_on of %s, and every hypothesis builds on something: link it where it belongs first, then unlink this one", ErrInvalidLink, spec.To, fromID)
		}
		if _, err := collect(ctx, tx, linkMatch(from.PlatformExperimentID, spec)+` MATCH (h)-[r:`+relTypes[spec.Type]+`]->(t) DELETE r`, params); err != nil {
			return nil, err
		}
		removed = true
		return nil, nil
	})
	if err != nil {
		return false, nil, wrap("graphdb.Unlink", err)
	}
	if out, err = s.outLinks(ctx, fromID); err != nil {
		return false, nil, err
	}
	return removed, out, nil
}

// lockedHypothesis takes the lock of hypothesis id's platform experiment, which never changes,
// and then reads the hypothesis within tx: what it answers no write of another can change before
// tx ends.
func lockedHypothesis(ctx context.Context, tx neo4j.ManagedTransaction, id string) (*domain.Hypothesis, error) {
	records, err := collect(ctx, tx, `MATCH (h:Hypothesis {id: $id}) RETURN h.platform_experiment_id`, map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	var d decoder
	pe := d.str(records[0].Values[0])
	if d.err != nil {
		return nil, d.err
	}
	if err := lockExperiment(ctx, tx, pe); err != nil {
		return nil, err
	}
	h, err := getHypothesis(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return h, nil
}
