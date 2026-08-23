package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

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

func needAccelerators(flavor domain.AcceleratorType, n int64) domain.Footprint {
	fp := domain.NewFootprint()
	fp.Add(domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: string(flavor)}, n)
	return fp
}

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
	observed := fakeObserved{elapsed: map[string]float64{
		"nearly-done":  9,
		"just-started": 0.1,
		"halfway":      5,
	}}
	store := &preemptStore{}
	l, settler := preemptLoop(store, observed)

	committed, err := l.preempt(context.Background(), needAccelerators(flavor, 1), candidates, burstVictim("preemptor", flavor))
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
		inner:  fakeObserved{elapsed: map[string]float64{"measurable": 4}},
		failed: map[string]bool{"unmeasurable": true},
	}
	store := &preemptStore{}
	l, _ := preemptLoop(store, observed)

	committed, err := l.preempt(context.Background(), needAccelerators(flavor, 1), candidates, burstVictim("preemptor", flavor))
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

	committed, err := l.preempt(context.Background(), needAccelerators(flavor, 1),
		[]*domain.Experiment{burstVictim("a", flavor)}, burstVictim("preemptor", flavor))
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
	l, settler := preemptLoop(store, fakeObserved{elapsed: map[string]float64{"a": 1, "b": 2}})

	committed, err := l.preempt(context.Background(), needAccelerators(flavor, 3), candidates, burstVictim("preemptor", flavor))
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
	l, _ := preemptLoop(store, fakeObserved{elapsed: map[string]float64{"a": 1, "b": 2, "c": 3}})

	committed, err := l.preempt(context.Background(), needAccelerators(flavor, 1), candidates, burstVictim("preemptor", flavor))
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

// TestPreemptIgnoresVictimsHoldingADifferentFlavor is preemptionContribution's rule seen from the
// outside: evicting an L40 job frees nothing for a job that needs an H100, so it must never be
// chosen to cover an H100 shortage — however little progress it has made.
func TestPreemptIgnoresVictimsHoldingADifferentFlavor(t *testing.T) {
	const wanted = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-h100-80gb-hbm3")
	const other = domain.AcceleratorType("nvidia.com/gpu.product=nvidia-l40")
	candidates := []*domain.Experiment{
		burstVictim("wrong-flavor", other), // least progress, so ranked first
		burstVictim("right-flavor", wanted),
	}
	store := &preemptStore{}
	l, _ := preemptLoop(store, fakeObserved{elapsed: map[string]float64{"wrong-flavor": 0.1, "right-flavor": 8}})

	committed, err := l.preempt(context.Background(), needAccelerators(wanted, 1), candidates, burstVictim("preemptor", wanted))
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
	l, _ := preemptLoop(store, fakeObserved{
		elapsed: map[string]float64{"victim": 9},
		stint:   map[string]float64{"victim": 4},
	})

	if _, err := l.preempt(context.Background(), needAccelerators(flavor, 1), []*domain.Experiment{victim}, burstVictim("preemptor", flavor)); err != nil {
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
	l, _ := preemptLoop(store, fakeObserved{elapsed: map[string]float64{"a": 1, "b": 2}})

	committed, err := l.preempt(context.Background(), needAccelerators(flavor, 2), candidates, burstVictim("preemptor", flavor))
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
	l, _ := preemptLoop(store, fakeObserved{})

	if committed, err := l.preempt(context.Background(), needAccelerators(flavor, 1), nil, burstVictim("p", flavor)); committed || err != nil {
		t.Fatalf("preempt with no candidates = (%v, %v), want (false, nil)", committed, err)
	}
	if committed, err := l.preempt(context.Background(), domain.NewFootprint(), []*domain.Experiment{burstVictim("a", flavor)}, burstVictim("p", flavor)); committed || err != nil {
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
