package graphdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Two links that each close no cycle but together close one through three hypotheses touch no
// common node, so only the per-experiment write lock keeps one of them out.
func TestConcurrentLinksCannotCloseAMultiHopCycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for round := range 10 {
		pe := testExperiment(t, s)
		x := register(t, s, pe, "agent-1", "x", onRoot(pe))
		z := register(t, s, pe, "agent-1", "z", onRoot(pe))
		y := register(t, s, pe, "agent-1", "y", []domain.LinkSpec{{Type: domain.LinkBuildsOn, To: x.ID}})
		links := [][2]string{{x.ID, z.ID}, {z.ID, y.ID}}
		errs := make([]error, len(links))
		var wg sync.WaitGroup
		for i, l := range links {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.AddLink(ctx, l[0], "agent-1", agentActor("agent-1"), domain.LinkSpec{Type: domain.LinkBuildsOn, To: l[1]})
			}()
		}
		wg.Wait()
		refused := 0
		for i, err := range errs {
			switch {
			case errors.Is(err, ErrInvalidLink):
				refused++
			case err != nil:
				t.Fatalf("round %d link %d: got = %v, want nil or %v", round, i, err, ErrInvalidLink)
			}
		}
		if refused != 1 {
			t.Fatalf("round %d refused links: got = %d, want 1", round, refused)
		}
		if n := count(t, s, fmt.Sprintf(`MATCH p = (h:Hypothesis {platform_experiment_id: '%s'})-[:BUILDS_ON *]->(h) RETURN count(p)`, pe)); n != 0 {
			t.Fatalf("round %d builds_on cycles: got = %d, want 0", round, n)
		}
	}
}

// Near-duplicates registered at the same moment do not share normalized text, so only the
// per-experiment write lock lets the later ones see the first and be held back.
func TestConcurrentNearDuplicateRegistrationsCreateOne(t *testing.T) {
	s := testStore(t)
	pe := testExperiment(t, s)
	texts := []string{
		"linear warmup over 500 steps stabilises early training",
		"linear warmup over 500 steps stabilises early training loss",
		"linear warmup over 500 steps stabilises early training runs",
		"a linear warmup over 500 steps stabilises early training",
		"linear warmup over 500 steps stabilises the early training",
		"linear warmup during 500 steps stabilises early training",
	}
	errs := make([]error, len(texts))
	var wg sync.WaitGroup
	for i, text := range texts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, _, errs[i] = s.Register(context.Background(), NewHypothesis{
				AgentID: fmt.Sprintf("agent-%d", i), PlatformExperimentID: pe,
				Text: text, Links: onRoot(pe),
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrSimilar) {
			t.Fatalf("Register %d: got = %v, want nil or %v", i, err, ErrSimilar)
		}
	}
	if stats, err := s.HypothesisStats(context.Background(), ListFilter{PlatformExperimentID: pe}); err != nil || stats.Total != 1 {
		t.Errorf("hypotheses in the experiment: got = (%v, %v), want (1, nil)", stats, err)
	}
}

// A link to a hypothesis that is being merged either lands before the merge, which moves it to the
// survivor, or after, which refuses it: no link ever points at a merged hypothesis.
func TestALinkRacingAMergeNeverPointsAtTheMergedHypothesis(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for round := range 3 {
		pe := testExperiment(t, s)
		into := register(t, s, pe, "agent-1", "warmup over five hundred steps", onRoot(pe))
		dup := register(t, s, pe, "agent-1", "cosine decay to zero", onRoot(pe))
		x := register(t, s, pe, "agent-2", "weight decay of a tenth", onRoot(pe))
		var mergeErr, linkErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, mergeErr = s.Merge(ctx, dup.ID, into.ID, agentActor("researcher-1"), "")
		}()
		go func() {
			defer wg.Done()
			_, linkErr = s.AddLink(ctx, x.ID, "agent-2", agentActor("agent-2"), domain.LinkSpec{Type: domain.LinkBuildsOn, To: dup.ID})
		}()
		wg.Wait()
		if mergeErr != nil {
			t.Fatalf("round %d Merge: got = %v, want nil", round, mergeErr)
		}
		if linkErr != nil && !errors.Is(linkErr, ErrInvalidLink) {
			t.Fatalf("round %d AddLink: got = %v, want nil or %v", round, linkErr, ErrInvalidLink)
		}
		if n := count(t, s, fmt.Sprintf(`MATCH (:Hypothesis)-[r]->(:Hypothesis {id: '%s'}) RETURN count(r)`, dup.ID)); n != 0 {
			t.Fatalf("round %d links to the merged hypothesis: got = %d, want 0", round, n)
		}
		want := int64(0)
		if linkErr == nil {
			want = 1
		}
		if n := count(t, s, fmt.Sprintf(`MATCH (:Hypothesis {id: '%s'})-[r:BUILDS_ON]->(:Hypothesis {id: '%s'}) RETURN count(r)`, x.ID, into.ID)); n != want {
			t.Fatalf("round %d builds_on the survivor: got = %d, want %d", round, n, want)
		}
	}
}

// race runs each of fns at once and answers their errors in order.
func race(fns ...func() error) []error {
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = fn()
		}()
	}
	wg.Wait()
	return errs
}

// three registers three hypotheses of distinct claims on the objective of a new platform
// experiment.
func three(t *testing.T, s *Store) (pe string, a, b, c *domain.Hypothesis) {
	t.Helper()
	pe = testExperiment(t, s)
	a = register(t, s, pe, "agent-1", "gradient clipping at norm one stabilises training", onRoot(pe))
	b = register(t, s, pe, "agent-2", "label smoothing of ten percent lifts validation accuracy", onRoot(pe))
	c = register(t, s, pe, "agent-3", "mixed precision halves the step time", onRoot(pe))
	return pe, a, b, c
}

func hypothesis(t *testing.T, s *Store, id string) *domain.Hypothesis {
	t.Helper()
	h, err := s.GetHypothesis(context.Background(), id)
	if err != nil || h == nil {
		t.Fatalf("GetHypothesis %s: got = %v, %v", id, h, err)
	}
	return h
}

// A verdict and a merge of one hypothesis: one of them lands and the other is refused, so a merged
// hypothesis never carries a verdict given after it was merged.
func TestAVerdictAndAMergeOfOneHypothesisDoNotBothLand(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for round := range 3 {
		_, a, b, _ := three(t, s)
		errs := race(
			func() error {
				_, _, err := s.UpdateStatus(ctx, a.ID, "agent-1", agentActor("agent-1"), domain.HypothesisConfirmed)
				return err
			},
			func() error {
				_, _, err := s.Merge(ctx, a.ID, b.ID, agentActor("researcher-1"), "same claim")
				return err
			})
		got := hypothesis(t, s, a.ID)
		switch {
		case errs[0] == nil && errors.Is(errs[1], ErrNotOpen) && got.Status == domain.HypothesisConfirmed:
		case errors.Is(errs[0], ErrCurated) && errs[1] == nil && got.Status == domain.HypothesisMerged:
		default:
			t.Fatalf("round %d: verdict %v, merge %v, status %s, want exactly one of them landed", round, errs[0], errs[1], got.Status)
		}
	}
}

// One hypothesis merged into two others at once is merged into one of them.
func TestTwoMergesOfOneHypothesisDoNotBothLand(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for round := range 3 {
		_, a, b, c := three(t, s)
		errs := race(
			func() error {
				_, _, err := s.Merge(ctx, a.ID, b.ID, agentActor("researcher-1"), "same claim")
				return err
			},
			func() error {
				_, _, err := s.Merge(ctx, a.ID, c.ID, agentActor("researcher-2"), "same claim")
				return err
			})
		got := hypothesis(t, s, a.ID)
		switch {
		case errs[0] == nil && errors.Is(errs[1], ErrNotOpen) && got.MergedInto == b.ID:
		case errors.Is(errs[0], ErrNotOpen) && errs[1] == nil && got.MergedInto == c.ID:
		default:
			t.Fatalf("round %d: merges %v and %v, merged into %q, want exactly one landed", round, errs[0], errs[1], got.MergedInto)
		}
	}
}

// A merged into B while B is merged into C: whichever order they land in, merged_into names a
// hypothesis that is not merged.
func TestMergesIntoAChainLeaveEveryMergedIntoOnOneThatIsNotMerged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for round := range 3 {
		pe, a, b, c := three(t, s)
		errs := race(
			func() error {
				_, _, err := s.Merge(ctx, a.ID, b.ID, agentActor("researcher-1"), "same claim")
				return err
			},
			func() error {
				_, _, err := s.Merge(ctx, b.ID, c.ID, agentActor("researcher-1"), "same claim")
				return err
			})
		for i, err := range errs {
			if err != nil && !errors.Is(err, ErrInvalidLink) {
				t.Fatalf("round %d merge %d: got = %v, want nil or %v", round, i, err, ErrInvalidLink)
			}
		}
		if errs[1] != nil {
			t.Fatalf("round %d: merging b into c: got = %v, want nil", round, errs[1])
		}
		if n := count(t, s, fmt.Sprintf(`MATCH (h:Hypothesis {platform_experiment_id: '%s'}), (m:Hypothesis {id: h.merged_into}) WHERE m.status = 'merged' RETURN count(h)`, pe)); n != 0 {
			t.Fatalf("round %d: hypotheses merged into a merged one: got = %d, want 0", round, n)
		}
		if errs[0] == nil && hypothesis(t, s, a.ID).MergedInto != c.ID {
			t.Fatalf("round %d: a merged into %q, want %s", round, hypothesis(t, s, a.ID).MergedInto, c.ID)
		}
	}
}

// A merge and a retirement of one hypothesis: one of them lands.
func TestAMergeAndARetirementOfOneHypothesisDoNotBothLand(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for round := range 3 {
		_, a, b, _ := three(t, s)
		errs := race(
			func() error {
				_, _, err := s.Merge(ctx, a.ID, b.ID, agentActor("researcher-1"), "same claim")
				return err
			},
			func() error { _, _, err := s.Retire(ctx, a.ID, agentActor("researcher-2"), "moot"); return err })
		got := hypothesis(t, s, a.ID)
		switch {
		case errs[0] == nil && errors.Is(errs[1], ErrNotOpen) && got.Status == domain.HypothesisMerged:
		case errors.Is(errs[0], ErrNotOpen) && errs[1] == nil && got.Status == domain.HypothesisRetired && got.MergedInto == "":
		default:
			t.Fatalf("round %d: merge %v, retirement %v, status %s, want exactly one landed", round, errs[0], errs[1], got.Status)
		}
	}
}

// What was merged into a hypothesis that is merged in turn names where it is tested now.
func TestMergingAMergeTargetMovesWhatWasMergedIntoIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, a, b, c := three(t, s)
	if _, _, err := s.Merge(ctx, a.ID, b.ID, agentActor("researcher-1"), "same claim"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Merge(ctx, b.ID, c.ID, agentActor("researcher-1"), "same claim"); err != nil {
		t.Fatal(err)
	}
	if got := hypothesis(t, s, a.ID); got.MergedInto != c.ID || got.Status != domain.HypothesisMerged {
		t.Errorf("a: got = %s into %q, want merged into %s", got.Status, got.MergedInto, c.ID)
	}
}
