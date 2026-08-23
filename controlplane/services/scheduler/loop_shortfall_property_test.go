package scheduler

import (
	"math/rand"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// preemptionShortfall is what turns "this job did not fit" into an actionable vector: preempt()
// picks victims whose combined footprint covers it, and the disbalance evictor uses it to explain
// which neighbour is the reason nothing fits. Two ways it can be wrong, both expensive:
//
//	under-report — preemption evicts victims that cover the reported shortage, the job still does
//	               not fit, and running work was destroyed for nothing
//	over-report  — more work is evicted than the job actually needed
//
// The property below targets the worst case of under-reporting: a shortfall of nothing at all for
// a job that demonstrably does not fit. That job is invisible to both remedies — preemption has no
// vector to cover and the evictor has no deficit to explain — so it sits queued indefinitely while
// the scheduler reports it as needing nothing. The function's own doc comment names this as the
// bug it was written to fix, which makes it exactly the thing a regression test should pin.

// clusterTotals sums per-node capacity into the cluster-wide Footprint that the tick derives from
// GetFlavorCapacity and hands to preemptionShortfall. Building it from the same per-node maps the
// placement check uses keeps the two views consistent, so a non-empty shortfall in the test can
// only come from a genuine deficit rather than from two disagreeing snapshots.
func clusterTotals(nodeAccel, nodeRes map[string]map[string]int64) domain.Footprint {
	fp := domain.NewFootprint()
	for _, capacity := range nodeAccel {
		for flavor, n := range capacity {
			fp.Add(domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: flavor}, n)
		}
	}
	for _, capacity := range nodeRes {
		for dimension, n := range capacity {
			kind, ok := nodeResourceKind(dimension)
			if !ok {
				continue
			}
			fp.Add(domain.ResourceKey{Kind: kind}, n)
		}
	}
	return fp
}

func positiveDimensions(fp domain.Footprint) int {
	n := 0
	for _, v := range fp {
		if v > 0 {
			n++
		}
	}
	return n
}

// TestPreemptionShortfallIsNeverEmptyForAJobThatDoesNotFit runs the placement check and the
// shortfall calculation over the same randomized layouts and asserts they never contradict each
// other: whenever placement says no, the shortfall must name at least one dimension to free.
func TestPreemptionShortfallIsNeverEmptyForAJobThatDoesNotFit(t *testing.T) {
	const iterations = 4000
	rnd := rand.New(rand.NewSource(20260906))

	var checked int
	for i := 0; i < iterations; i++ {
		c := generateCase(rnd)

		footprint, err := c.exp.Job.Footprint(c.exp.AcceleratorType)
		if err != nil {
			continue // a generated spec that cannot be costed is not a placement question
		}
		if reservePlacement(cloneNodeCapacity(c.nodeAccel), cloneNodeCapacity(c.nodeRes), nil, c.exp) {
			continue // fits, so there is nothing to report
		}
		checked++

		got := preemptionShortfall(clusterTotals(c.nodeAccel, c.nodeRes), c.nodeAccel, c.nodeRes, nil, c.exp, footprint)
		if positiveDimensions(got) == 0 {
			t.Fatalf("case %d: the job does not fit, yet the shortfall is empty — preemption gets no vector to cover and the evictor no deficit to explain, so this job stays queued forever while reported as needing nothing\nshortfall: %s\n%s",
				i, footprintStr(got), c.describe())
		}
	}
	// Guards the test itself: if the generator drifted to producing only placeable jobs, the loop
	// above would pass without ever having asserted anything.
	if checked < iterations/20 {
		t.Fatalf("only %d of %d generated cases were infeasible — the generator is no longer producing the layouts this test exists to check", checked, iterations)
	}
	t.Logf("checked %d infeasible layouts", checked)
}

// TestPreemptionShortfallCountsEveryRankNotJustOne is the minimal reproduction of the bug the
// property above caught, kept by name so a regression is self-describing.
//
// The job is two ranks of 2Gi disk each. The cluster holds 4Gi in total, so the cluster-level
// check is satisfied, and one node does have a free 2Gi, so "can some node host a rank?" is also
// satisfied — but the second rank has nowhere to go, and the job can never be placed. Asking only
// whether one node had room reported this as needing nothing at all.
func TestPreemptionShortfallCountsEveryRankNotJustOne(t *testing.T) {
	exp := groupedExperiment(domain.JobGroup{Name: "worker", Replicas: 2, Storage: "2Gi"})
	nodeAccel := map[string]map[string]int64{"node-a": {}, "node-b": {}, "node-c": {}}
	nodeRes := map[string]map[string]int64{
		"node-a": {domain.NodeResourceStorageBytes: 1 << 30},
		"node-b": {domain.NodeResourceStorageBytes: 1 << 30},
		"node-c": {domain.NodeResourceStorageBytes: 2 << 30},
	}

	// Establish the premise rather than assuming it: the job really is unplaceable.
	if reservePlacement(cloneNodeCapacity(nodeAccel), cloneNodeCapacity(nodeRes), nil, exp) {
		t.Fatal("fixture is wrong: two ranks of 2Gi cannot both be placed on 1Gi+1Gi+2Gi")
	}
	footprint, err := exp.Job.Footprint(exp.AcceleratorType)
	if err != nil {
		t.Fatalf("Footprint() = %v", err)
	}
	// ...and that the cluster-level view alone sees no problem, which is why the per-node pass has
	// to be the one that catches it.
	if len(shortfall(clusterTotals(nodeAccel, nodeRes), footprint)) != 0 {
		t.Fatal("fixture is wrong: this case is only interesting while the cluster-wide totals suffice")
	}

	got := preemptionShortfall(clusterTotals(nodeAccel, nodeRes), nodeAccel, nodeRes, nil, exp, footprint)
	storage := got[domain.ResourceKey{Kind: domain.ResourceKindStorage}]
	if storage <= 0 {
		t.Fatalf("shortfall = %s, want a positive storage deficit: the second rank has no host", footprintStr(got))
	}
	// One rank's worth is what has to be freed on one node, not the whole job's 4Gi.
	if storage > 2<<30 {
		t.Fatalf("shortfall wants %d bytes of storage freed, more than the one rank that cannot be placed needs", storage)
	}
}

// TestPreemptionShortfallNeverReportsUnrequestedDimensions pins the other direction: the vector
// preempt() is handed must only name dimensions the job actually asked for. A shortfall naming a
// dimension the job does not request would send preemption hunting for victims holding a resource
// that would not help, evicting live work that frees nothing relevant.
func TestPreemptionShortfallNeverReportsUnrequestedDimensions(t *testing.T) {
	const iterations = 4000
	rnd := rand.New(rand.NewSource(20260907))

	for i := 0; i < iterations; i++ {
		c := generateCase(rnd)
		footprint, err := c.exp.Job.Footprint(c.exp.AcceleratorType)
		if err != nil {
			continue
		}
		got := preemptionShortfall(clusterTotals(c.nodeAccel, c.nodeRes), c.nodeAccel, c.nodeRes, nil, c.exp, footprint)
		for key, amount := range got {
			if amount <= 0 {
				continue
			}
			if footprint[key] <= 0 {
				t.Fatalf("case %d: shortfall names %s/%s (%d), which the job never requested\nshortfall: %s\nfootprint: %s\n%s",
					i, key.Kind, key.Flavor, amount, footprintStr(got), footprintStr(footprint), c.describe())
			}
		}
	}
}

// TestPreemptionShortfallNeverExceedsTheJobsOwnFootprint bounds over-reporting. No eviction plan
// should ever need to free more of a dimension than the whole job consumes: the cluster only has
// to make room for this job, not for a multiple of it. A shortfall above that ceiling evicts more
// running work than the admission could possibly require.
func TestPreemptionShortfallNeverExceedsTheJobsOwnFootprint(t *testing.T) {
	const iterations = 4000
	rnd := rand.New(rand.NewSource(20260908))

	for i := 0; i < iterations; i++ {
		c := generateCase(rnd)
		footprint, err := c.exp.Job.Footprint(c.exp.AcceleratorType)
		if err != nil {
			continue
		}
		got := preemptionShortfall(clusterTotals(c.nodeAccel, c.nodeRes), c.nodeAccel, c.nodeRes, nil, c.exp, footprint)
		for key, amount := range got {
			if amount > footprint[key] {
				t.Fatalf("case %d: shortfall wants %d of %s/%s freed, but the whole job only uses %d — preemption would evict more than admitting this job can require\nshortfall: %s\n%s",
					i, amount, key.Kind, key.Flavor, footprint[key], footprintStr(got), c.describe())
			}
		}
	}
}

// TestShortfallBasics covers the plain cluster-level helper directly, including the degenerate
// inputs the randomized layouts above are unlikely to produce.
func TestShortfallBasics(t *testing.T) {
	cpu := domain.ResourceKey{Kind: domain.ResourceKindCPU}
	accel := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: h100}

	t.Run("reports only the deficit, not the whole request", func(t *testing.T) {
		avail := domain.Footprint{cpu: 3000, accel: 1}
		want := domain.Footprint{cpu: 4000, accel: 4}
		got := shortfall(avail, want)
		if got[cpu] != 1000 || got[accel] != 3 {
			t.Fatalf("shortfall = %s, want cpu=1000 accel=3", footprintStr(got))
		}
	})

	t.Run("a satisfied request produces no dimensions at all", func(t *testing.T) {
		got := shortfall(domain.Footprint{cpu: 9000, accel: 8}, domain.Footprint{cpu: 1000, accel: 1})
		if len(got) != 0 {
			t.Fatalf("shortfall = %s, want empty for a request that already fits", footprintStr(got))
		}
	})

	t.Run("a dimension absent from capacity counts as zero available", func(t *testing.T) {
		// Failing closed matters here: a cluster that has not reported a dimension must read as
		// having none of it, never as having enough.
		got := shortfall(domain.Footprint{}, domain.Footprint{accel: 2})
		if got[accel] != 2 {
			t.Fatalf("shortfall = %s, want the full request when capacity reports nothing", footprintStr(got))
		}
	})

	t.Run("surplus in one dimension never offsets a deficit in another", func(t *testing.T) {
		got := shortfall(domain.Footprint{cpu: 100000, accel: 0}, domain.Footprint{cpu: 1000, accel: 2})
		if got[accel] != 2 || got[cpu] != 0 {
			t.Fatalf("shortfall = %s, want accel=2 only — abundant CPU must not mask a missing accelerator", footprintStr(got))
		}
	})
}
