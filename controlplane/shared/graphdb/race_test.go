package graphdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
				_, errs[i] = s.AddLink(ctx, l[0], "agent-1", "", domain.LinkSpec{Type: domain.LinkBuildsOn, To: l[1]}, stampAt(1))
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
				Source: domain.HypothesisSourceAgent, AgentID: fmt.Sprintf("agent-%d", i), PlatformExperimentID: pe,
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
