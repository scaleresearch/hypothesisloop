package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// preempt() is the only thing in the loop that stops work the queue never asked to stop, and every
// decision it makes turns on observed runtime read from the metrics store. While that read was a
// package-level call taking a database URL, none of this could be tested at all — which is why the
// rules it documents so carefully (skip the unrankable candidate rather than evicting it first;
// never execute a partial plan) had nothing holding them in place.

type preemptStore struct {
	LoopStore
	requeued  []string
	remaining map[string]float64
	refuse    map[string]bool // victims that reached a terminal status before the requeue landed
	err       error
}

func (s *preemptStore) RequeuePreempted(_ context.Context, id string, remainingHours, _ float64) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.refuse[id] {
		return false, nil
	}
	if s.remaining == nil {
		s.remaining = map[string]float64{}
	}
	s.requeued = append(s.requeued, id)
	s.remaining[id] = remainingHours
	return true, nil
}

type recordingSettler struct{ settled []string }

func (s *recordingSettler) Settle(_ context.Context, exp *domain.Experiment) error {
	s.settled = append(s.settled, exp.ID)
	return nil
}

// burstVictim is a running burst job holding one accelerator of the given flavor.
func burstVictim(id string, flavor domain.AcceleratorType) *domain.Experiment {
	return &domain.Experiment{
		ID:                     id,
		ClusterName:            "cluster-a",
		CapacityTier:           domain.CapacityBurst,
		Status:                 domain.StatusRunning,
		AcceleratorType:        flavor,
		AcceleratorCount:       1,
		EstimatedDurationHours: 10,
		EstimatedCostAccH:      10,
		CreatedAt:              time.Now().Add(-time.Hour),
		Job:                    domain.JobSpec{AcceleratorType: flavor, AcceleratorCount: 1, NumNodes: 1},
	}
}

func preemptLoop(store LoopStore, observed ObservedState) (*Loop, *recordingSettler) {
	settler := &recordingSettler{}
	l := &Loop{store: store, observed: observed, settler: settler, logger: zap.NewNop()}
	return l, settler
}

// preemptorNeeding is a queued one-node job asking for n accelerators of flavor.
func preemptorNeeding(flavor domain.AcceleratorType, n int) *domain.Experiment {
	exp := burstVictim("preemptor", flavor)
	exp.AcceleratorCount, exp.Job.AcceleratorCount = n, n
	return exp
}

// preemptFor runs preempt for preemptor against a cluster whose nodes report free accelerators
// of the preemptor's flavor.
func preemptFor(l *Loop, free map[string]int64, candidates []*domain.Experiment, preemptor *domain.Experiment) (bool, error) {
	avail := domain.NewFootprint()
	nodeAvail := map[string]map[string]int64{}
	for node, n := range free {
		avail.Add(domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: string(preemptor.AcceleratorType)}, n)
		nodeAvail[node] = map[string]int64{string(preemptor.AcceleratorType): n}
	}
	return l.preempt(context.Background(), avail, nodeAvail, nil, nil, candidates, preemptor)
}

func onNodeA(observed fakeObserved, ids ...string) fakeObserved {
	observed.node = observedOnNode(ids, "node-a").node
	return observed
}

var fullNodeA = map[string]int64{"node-a": 0}

// TestPreemptEvictsTheLeastProgressedVictim pins the ranking rule: whoever has done the least
// observed work loses, because that is the least work destroyed. Every candidate here is identical
// apart from how long it has been observed running, so nothing else can explain the choice.
func TestPreemptEvictsTheLeastProgressedVictim(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	candidates := []*domain.Experiment{
		burstVictim("nearly-done", flavor),
		burstVictim("just-started", flavor),
		burstVictim("halfway", flavor),
	}
	observed := onNodeA(fakeObserved{elapsed: map[string]float64{
		"nearly-done":  9,
		"just-started": 0.1,
		"halfway":      5,
	}}, "nearly-done", "just-started", "halfway")
	store := &preemptStore{}
	l, settler := preemptLoop(store, observed)

	committed, err := preemptFor(l, fullNodeA, candidates, preemptorNeeding(flavor, 1))
	if err != nil {
		t.Fatalf("preempt: %v", err)
	}
	if !committed {
		t.Fatal("preempt did not commit a plan although one victim covers the shortage")
	}
	if len(store.requeued) != 1 || store.requeued[0] != "just-started" {
		t.Fatalf("requeued = %v, want exactly [just-started]: the least observed progress is evicted first", store.requeued)
	}
	// The requeued victim's already-burned hours have to land somewhere, or they are counted
	// nowhere until the job eventually terminates.
	if len(settler.settled) != 1 || settler.settled[0] != "just-started" {
		t.Fatalf("settled = %v, want [just-started]", settler.settled)
	}
}

// TestPreemptSkipsUnrankableCandidatesRatherThanEvictingThemFirst is the regression for the rule
// preempt() documents most emphatically. A candidate whose metrics query fails cannot be ranked;
// treating it as zero observed hours would sort it to the front and make the job nobody can
// measure the first one destroyed. It must be dropped from consideration instead.
func TestPreemptSkipsUnrankableCandidatesRatherThanEvictingThemFirst(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	candidates := []*domain.Experiment{
		burstVictim("unmeasurable", flavor),
		burstVictim("measurable", flavor),
	}
	// unmeasurable is absent from elapsed AND fails its query; measurable reports real progress.
	// Ranked as zero, unmeasurable would be evicted first — the exact bug being guarded.
	observed := failingFor{
		inner:  onNodeA(fakeObserved{elapsed: map[string]float64{"measurable": 4}}, "unmeasurable", "measurable"),
		failed: map[string]bool{"unmeasurable": true},
	}
	store := &preemptStore{}
	l, _ := preemptLoop(store, observed)

	committed, err := preemptFor(l, fullNodeA, candidates, preemptorNeeding(flavor, 1))
	if err != nil {
		t.Fatalf("preempt: %v", err)
	}
	if !committed {
		t.Fatal("preempt should still have committed using the one candidate it could rank")
	}
	for _, id := range store.requeued {
		if id == "unmeasurable" {
			t.Fatalf("requeued = %v: a candidate whose metrics query failed was evicted, and ranking it as zero progress makes the unmeasurable job the first victim every time", store.requeued)
		}
	}
	if len(store.requeued) != 1 || store.requeued[0] != "measurable" {
		t.Fatalf("requeued = %v, want [measurable]", store.requeued)
	}
}

// TestPreemptDoesNothingWhenNoCandidateCanBeRanked continues the same rule to its limit: one
// broken metrics series must not block preemption platform-wide, and with nothing rankable there
// is simply no plan. Reporting an error here would abort the caller's whole tick.
func TestPreemptDoesNothingWhenNoCandidateCanBeRanked(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	store := &preemptStore{}
	l, _ := preemptLoop(store, fakeObserved{err: errors.New("metrics store unreachable")})

	committed, err := preemptFor(l, fullNodeA,
		[]*domain.Experiment{burstVictim("a", flavor)}, preemptorNeeding(flavor, 1))
	if err != nil {
		t.Fatalf("preempt returned an error for an unreachable metrics store, which aborts the caller's tick: %v", err)
	}
	if committed {
		t.Fatal("preempt committed a plan built from candidates it could not rank")
	}
	if len(store.requeued) != 0 {
		t.Fatalf("requeued = %v, want nothing", store.requeued)
	}
}

// TestPreemptNeverExecutesAPartialPlan guards the "never disrupt jobs for a partial plan" rule.
// The shortage needs three accelerators and only two are available across every candidate, so
// evicting both would destroy live work and still leave the preemptor unable to run.
func TestPreemptNeverExecutesAPartialPlan(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	candidates := []*domain.Experiment{burstVictim("a", flavor), burstVictim("b", flavor)}
	store := &preemptStore{}
	l, settler := preemptLoop(store, onNodeA(fakeObserved{elapsed: map[string]float64{"a": 1, "b": 2}}, "a", "b"))

	committed, err := preemptFor(l, fullNodeA, candidates, preemptorNeeding(flavor, 3))
	if err != nil {
		t.Fatalf("preempt: %v", err)
	}
	if committed {
		t.Fatal("preempt reported a committed plan that cannot cover the shortage")
	}
	if len(store.requeued) != 0 {
		t.Fatalf("requeued = %v, want nothing: two accelerators cannot cover a shortage of three, so evicting them destroys work for nothing", store.requeued)
	}
	if len(settler.settled) != 0 {
		t.Fatalf("settled = %v, want nothing", settler.settled)
	}
}

// TestPreemptEvictsNoMoreThanTheShortageNeeds exercises the fill-back pass. Three candidates are
// available and one suffices; selecting a victim that the plan does not need is work destroyed for
// no admission.
func TestPreemptEvictsNoMoreThanTheShortageNeeds(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	candidates := []*domain.Experiment{
		burstVictim("a", flavor), burstVictim("b", flavor), burstVictim("c", flavor),
	}
	store := &preemptStore{}
	l, _ := preemptLoop(store, onNodeA(fakeObserved{elapsed: map[string]float64{"a": 1, "b": 2, "c": 3}}, "a", "b", "c"))

	committed, err := preemptFor(l, fullNodeA, candidates, preemptorNeeding(flavor, 1))
	if err != nil {
		t.Fatalf("preempt: %v", err)
	}
	if !committed {
		t.Fatal("preempt found no plan although any single victim covers the shortage")
	}
	if len(store.requeued) != 1 {
		t.Fatalf("requeued = %v, want exactly one victim for a one-accelerator shortage", store.requeued)
	}
}

// TestPreemptIgnoresVictimsHoldingADifferentFlavor: evicting an L40 job frees nothing for a job that needs an H100, so it must never be
// chosen to cover an H100 shortage — however little progress it has made.
func TestPreemptIgnoresVictimsHoldingADifferentFlavor(t *testing.T) {
	const wanted = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	const other = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-l40")
	candidates := []*domain.Experiment{
		burstVictim("wrong-flavor", other), // least progress, so ranked first
		burstVictim("right-flavor", wanted),
	}
	store := &preemptStore{}
	l, _ := preemptLoop(store, onNodeA(fakeObserved{elapsed: map[string]float64{"wrong-flavor": 0.1, "right-flavor": 8}}, "wrong-flavor", "right-flavor"))

	committed, err := preemptFor(l, fullNodeA, candidates, preemptorNeeding(wanted, 1))
	if err != nil {
		t.Fatalf("preempt: %v", err)
	}
	if !committed {
		t.Fatal("preempt found no plan although the right-flavor victim covers the shortage")
	}
	for _, id := range store.requeued {
		if id == "wrong-flavor" {
			t.Fatalf("requeued = %v: evicting a job holding a different accelerator frees nothing the preemptor can use", store.requeued)
		}
	}
}

// TestPreemptRescalesTheVictimAgainstItsOwnStint pins the accounting that decides how much work a
// requeued job is still believed to have left. The victim's estimate is 10 hours and this stint
// observed 4, so 6 remain — computed against the stint, never against lifetime elapsed, or a job
// preempted twice has the first stint's hours charged again and its estimate collapses while most
// of the work is still ahead.
func TestPreemptRescalesTheVictimAgainstItsOwnStint(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	victim := burstVictim("victim", flavor)
	store := &preemptStore{}
	// Lifetime elapsed (9) is deliberately far from this stint's (4): only the stint may be used.
	l, _ := preemptLoop(store, onNodeA(fakeObserved{
		elapsed: map[string]float64{"victim": 9},
		stint:   map[string]float64{"victim": 4},
	}, "victim"))

	if _, err := preemptFor(l, fullNodeA, []*domain.Experiment{victim}, preemptorNeeding(flavor, 1)); err != nil {
		t.Fatalf("preempt: %v", err)
	}
	if got := store.remaining["victim"]; got != 6 {
		t.Fatalf("remaining hours = %v, want 6 (10h estimate minus this stint's 4 observed hours, not its 9 lifetime hours)", got)
	}
}

// TestPreemptReportsAPartiallyExecutedPlanAsAnError covers the case the code calls the worst of
// both worlds: some victims have already been requeued, so the measured shortage is stale, yet the
// plan did not complete. It must surface as an error so the caller stands down rather than letting
// the disbalance evictor terminate more live work against the same stale numbers.
func TestPreemptReportsAPartiallyExecutedPlanAsAnError(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	candidates := []*domain.Experiment{burstVictim("a", flavor), burstVictim("b", flavor)}
	// "b" reaches a terminal status between selection and requeue, so its requeue is refused.
	store := &preemptStore{refuse: map[string]bool{"b": true}}
	l, _ := preemptLoop(store, onNodeA(fakeObserved{elapsed: map[string]float64{"a": 1, "b": 2}}, "a", "b"))

	committed, err := preemptFor(l, fullNodeA, candidates, preemptorNeeding(flavor, 2))
	if err == nil {
		t.Fatal("a partially executed plan must be reported as an error so the caller stands down")
	}
	if committed {
		t.Fatal("committed must be false for a plan that did not fully execute")
	}
}

// TestPreemptDoesNothingWithoutCandidatesOrShortage covers the degenerate entries.
func TestPreemptDoesNothingWithoutCandidatesOrShortage(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	store := &preemptStore{}
	l, _ := preemptLoop(store, onNodeA(fakeObserved{}, "a"))

	if committed, err := preemptFor(l, fullNodeA, nil, preemptorNeeding(flavor, 1)); committed || err != nil {
		t.Fatalf("preempt with no candidates = (%v, %v), want (false, nil)", committed, err)
	}
	if committed, err := preemptFor(l, map[string]int64{"node-a": 1}, []*domain.Experiment{burstVictim("a", flavor)}, preemptorNeeding(flavor, 1)); committed || err != nil {
		t.Fatalf("preempt with no shortage = (%v, %v), want (false, nil)", committed, err)
	}
	if len(store.requeued) != 0 {
		t.Fatalf("requeued = %v, want nothing", store.requeued)
	}
}

// failingFor answers normally except for the experiments named in failed, letting a test mix
// rankable and unrankable candidates in one pass.
type failingFor struct {
	inner  fakeObserved
	failed map[string]bool
}

func (f failingFor) ObservedElapsedHours(ctx context.Context, id string, createdAt, now time.Time) (float64, error) {
	if f.failed[id] {
		return 0, errors.New("no series for " + id)
	}
	return f.inner.ObservedElapsedHours(ctx, id, createdAt, now)
}

func (f failingFor) ObservedStintElapsedHours(ctx context.Context, id string, createdAt, now time.Time) (float64, error) {
	if f.failed[id] {
		return 0, errors.New("no series for " + id)
	}
	return f.inner.ObservedStintElapsedHours(ctx, id, createdAt, now)
}

func (f failingFor) LatestExperimentNode(ctx context.Context, id string, createdAt, now time.Time) (string, bool, error) {
	if f.failed[id] {
		return "", false, errors.New("no series for " + id)
	}
	return f.inner.LatestExperimentNode(ctx, id, createdAt, now)
}

func TestSubmitErrorLevelLogsALostCapacityRaceAsInfo(t *testing.T) {
	if got := submitErrorLevel(fmt.Errorf("submit: %w", errAdmissionCapacityChanged)); got != zapcore.InfoLevel {
		t.Errorf("lost capacity race: got = %v, want info", got)
	}
	if got := submitErrorLevel(errors.New("create workload: boom")); got != zapcore.ErrorLevel {
		t.Errorf("other submit failure: got = %v, want error", got)
	}
}

func TestPreemptNeverReservesMoreThanTheStintLeft(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	for name, tc := range map[string]struct{ estimate, stint, want float64 }{
		"a remainder under the floor is kept":      {1, 0.9, 0.1},
		"a stint past its estimate gets the floor": {1, 1.2, domain.MinRemainingHours},
		"the floor never exceeds the estimate":     {0.1, 0.2, 0.1},
	} {
		t.Run(name, func(t *testing.T) {
			victim := burstVictim("victim", flavor)
			victim.EstimatedDurationHours = tc.estimate
			store := &preemptStore{}
			l, _ := preemptLoop(store, onNodeA(fakeObserved{stint: map[string]float64{"victim": tc.stint}}, "victim"))

			if _, err := preemptFor(l, fullNodeA, []*domain.Experiment{victim}, preemptorNeeding(flavor, 1)); err != nil {
				t.Fatalf("preempt: %v", err)
			}
			if got := store.remaining["victim"]; got < tc.want-1e-9 || got > tc.want+1e-9 {
				t.Fatalf("remaining = %v, want %v", got, tc.want)
			}
		})
	}
}

type quotaRefusal struct{}

func (quotaRefusal) Error() string           { return "need 0.15 additional accelerator_hours" }
func (quotaRefusal) InsufficientQuota() bool { return true }

func TestAQuotaRefusalAtClaimHasItsOwnReason(t *testing.T) {
	err := fmt.Errorf("reserve selected accelerator flavor: %w", quotaRefusal{})
	if got, want := notAdmittedReasonForSubmitError(err), "insufficient_quota: need 0.15 additional accelerator_hours"; got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if got := submitErrorLevel(err); got != zapcore.InfoLevel {
		t.Fatalf("level = %v, want info", got)
	}
}

// The job needs four accelerators on one node. node-a has two free and node-b none, so only a
// victim on node-a makes a node able to host it, whatever the ranking says.
func TestPreemptTakesOnlyVictimsOnANodeThatCanThenHostTheJob(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	twoOn := func(id string) *domain.Experiment {
		v := burstVictim(id, flavor)
		v.AcceleratorCount, v.Job.AcceleratorCount = 2, 2
		return v
	}
	free := map[string]int64{"node-a": 2, "node-b": 0}

	t.Run("the victim on the hosting node is taken", func(t *testing.T) {
		store := &preemptStore{}
		l, _ := preemptLoop(store, fakeObserved{
			elapsed: map[string]float64{"on-b": 0.1, "on-a": 8},
			node:    map[string]string{"on-b": "node-b", "on-a": "node-a"},
		})
		committed, err := preemptFor(l, free, []*domain.Experiment{twoOn("on-b"), twoOn("on-a")}, preemptorNeeding(flavor, 4))
		if err != nil || !committed {
			t.Fatalf("preempt = (%v, %v), want a committed plan", committed, err)
		}
		if len(store.requeued) != 1 || store.requeued[0] != "on-a" {
			t.Fatalf("requeued = %v, want [on-a]", store.requeued)
		}
	})

	t.Run("no victim is taken when no node could then host the job", func(t *testing.T) {
		store := &preemptStore{}
		l, _ := preemptLoop(store, fakeObserved{node: map[string]string{"on-b": "node-b"}})
		committed, err := preemptFor(l, free, []*domain.Experiment{twoOn("on-b")}, preemptorNeeding(flavor, 4))
		if err != nil || committed || len(store.requeued) != 0 {
			t.Fatalf("preempt = (%v, %v), requeued %v: the cluster total would cover the job and no node would", committed, err, store.requeued)
		}
	})

	t.Run("a victim whose node is unknown is spared", func(t *testing.T) {
		store := &preemptStore{}
		l, _ := preemptLoop(store, fakeObserved{})
		committed, err := preemptFor(l, free, []*domain.Experiment{twoOn("unattributed")}, preemptorNeeding(flavor, 4))
		if err != nil || committed || len(store.requeued) != 0 {
			t.Fatalf("preempt = (%v, %v), requeued %v, want nothing taken", committed, err, store.requeued)
		}
	})
}

// One node is recorded per experiment, so a gang is proven to hold one rank on it and nothing is
// known about where its other ranks are.
func TestPreemptCreditsAGangOneRankOnItsRecordedNode(t *testing.T) {
	const flavor = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	gang := func() *domain.Experiment {
		g := burstVictim("gang", flavor)
		g.AcceleratorCount = 4
		g.Job.AcceleratorCount, g.Job.NumNodes = 2, 2
		return g
	}
	full := map[string]int64{"node-a": 0, "node-b": 0}
	for name, tc := range map[string]struct {
		need      int
		committed bool
	}{
		"a job the proven rank makes room for preempts the gang": {2, true},
		"a job needing both ranks on one node does not":          {4, false},
	} {
		t.Run(name, func(t *testing.T) {
			store := &preemptStore{}
			l, _ := preemptLoop(store, onNodeA(fakeObserved{}, "gang"))
			committed, err := preemptFor(l, full, []*domain.Experiment{gang()}, preemptorNeeding(flavor, tc.need))
			if err != nil || committed != tc.committed || (len(store.requeued) == 1) != tc.committed {
				t.Fatalf("preempt = (%v, %v), requeued %v, want committed %v", committed, err, store.requeued, tc.committed)
			}
		})
	}
}
