package db

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var claimLoadL40 = domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower("nvidia.com/gpu.product=NVIDIA-L40")}

// Eight admission passes claim at once, as replicas through a failover would, each job on either
// of two clusters, checking room as the scheduler does: the cluster's total less what desired
// state holds there, read fresh under the claim's lock. No cluster is claimed past what it has
// and no job is placed twice.
func TestClaimsRacingForTwoClustersNeverOvercommitOrPlaceAJobTwice(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	// Each claim holds a connection while its check reads on another; a pool smaller than the
	// passes would wait on itself. Replicas each have their own.
	pool, err := NewPool(ctx, Config{DSN: testDSN(), MaxConns: 20, MinConns: 1, Timeouts: testTimeouts})
	if err != nil {
		t.Skipf("no database at %s: %v", testDSN(), err)
	}
	t.Cleanup(pool.Close)
	w.exps = NewExperimentsStore(pool)
	second := "of-a2-" + w.suffix
	if _, err := w.clusters.RegisterCluster(ctx, second, w.orgA); err != nil {
		t.Fatal(err)
	}
	capacity := map[string]int64{w.ofA: 7, second: 12}
	rng := rand.New(rand.NewPCG(1, 2))
	var ids []string
	for i := range 60 {
		id := fmt.Sprintf("race-%d-%s", i, w.suffix)
		seedExperimentForCapTest(t, w.pool, id, w.agent, w.peA, 1+rng.IntN(3), domain.StatusQueued)
		ids = append(ids, id)
	}
	fits := func(cluster string, count int64) func(context.Context, []*domain.Experiment) (bool, error) {
		return func(ctx context.Context, _ []*domain.Experiment) (bool, error) {
			desired, err := w.exps.SumDesiredFootprintByCluster(ctx)
			if err != nil {
				return false, err
			}
			return capacity[cluster]-desired[cluster][claimLoadL40] >= count, nil
		}
	}

	var mu sync.Mutex
	claims := map[string]int{}
	var wg sync.WaitGroup
	start := time.Now()
	for pass := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order := rand.New(rand.NewPCG(uint64(pass), 3)).Perm(len(ids))
			for _, i := range order {
				exp, err := w.exps.GetExperiment(ctx, ids[i])
				if err != nil {
					t.Error(err)
					return
				}
				for _, cluster := range []string{w.ofA, second} {
					claimed, err := w.exps.ClaimSubmitted(ctx, exp.ID, cluster, &domain.JobSpec{Image: "registry.test/job:1", AcceleratorCount: exp.AcceleratorCount}, fits(cluster, int64(exp.AcceleratorCount)))
					if err != nil {
						t.Error(err)
						return
					}
					if claimed {
						mu.Lock()
						claims[exp.ID]++
						mu.Unlock()
						break
					}
				}
			}
		}()
	}
	wg.Wait()
	t.Logf("8 passes over %d jobs claimed %d in %v", len(ids), len(claims), time.Since(start))

	for id, n := range claims {
		if n > 1 {
			t.Errorf("%s was placed %d times", id, n)
		}
	}
	desired, err := w.exps.SumDesiredFootprintByCluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for cluster, has := range capacity {
		held := desired[cluster][claimLoadL40]
		if held > has {
			t.Errorf("%s holds %d of %d", cluster, held, has)
		}
		if held < has-2 {
			t.Errorf("%s holds %d of %d with jobs of at most 3 left queued", cluster, held, has)
		}
	}
}

// The reads one admission pass makes over a queue of 600 rows and six agents, timed against
// Postgres: one list of the queue, one read per agent of the cut, and the reads of desired state.
func TestTheReadsOfAPassOverHundredsOfQueuedRowsAreQuick(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(w.pool)
	agents := []string{w.agent}
	for i := range 5 {
		agent := fmt.Sprintf("agent-%d-%s", i, w.suffix)
		createTestAgent(t, w.pool, agent)
		agents = append(agents, agent)
	}
	for i := range 600 {
		seedExperimentForCapTest(t, w.pool, fmt.Sprintf("queue-%d-%s", i, w.suffix), agents[i%len(agents)], w.peA, 1, domain.StatusQueued)
	}
	for i := range 40 {
		w.placed(t, fmt.Sprintf("placed-%d", i), w.peA, w.ofA, domain.StatusRunning)
	}

	timed := func(name string, read func() error) time.Duration {
		t.Helper()
		start := time.Now()
		if err := read(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		took := time.Since(start)
		t.Logf("%s: %v", name, took)
		return took
	}
	var queued []*domain.Experiment
	total := timed("list queued", func() (err error) {
		queued, err = w.exps.ListQueuedExperiments(ctx)
		return err
	})
	if len(queued) < 600 {
		t.Fatalf("listed %d queued, want at least 600", len(queued))
	}
	total += timed("cut, once per agent", func() error {
		for _, agent := range agents {
			if _, err := pes.IsAgentCut(ctx, w.peA, agent); err != nil {
				return err
			}
		}
		return nil
	})
	total += timed("desired footprint by cluster", func() error {
		_, err := w.exps.SumDesiredFootprintByCluster(ctx)
		return err
	})
	total += timed("jobs holding capacity", func() error {
		_, err := w.exps.ListCapacityClaimedExperiments(ctx)
		return err
	})
	total += timed("registered clusters", func() error {
		_, err := w.clusters.RegisteredClusters(ctx)
		return err
	})
	total += timed("mark held jobs", func() error { return w.exps.MarkHeldJobs(ctx, 5*time.Minute) })
	t.Logf("the reads of one pass: %v", total)
	timed("for comparison, the cut once per queued job", func() error {
		for _, exp := range queued[:600] {
			if _, err := pes.IsAgentCut(ctx, exp.PlatformExperimentID, exp.AgentID); err != nil {
				return err
			}
		}
		return nil
	})
	if total > 2*time.Second {
		t.Errorf("the reads of one pass over %d queued rows took %v", len(queued), total)
	}
}
