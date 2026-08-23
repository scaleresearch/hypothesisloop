package settlement

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// These cover over-billing: a settlement that charges for MORE than the job observably consumed.
//
// They exist because the e2e preemption scenario used to carry that job in a settled-cost band,
// and could not. Its ceiling had to allow for what a preemption costs in wall clock — checkpoint
// write, teardown, re-admission, container start — and measured on a loaded cluster that overhead
// exceeded the job's own runtime. A ceiling wide enough not to fail correct runs was far too wide
// to catch a doubled charge. The invariant is not a duration at all, so it does not belong in a
// test that has to run one: settlement is a pure function of observed hours and the job's own
// per-hour rate, and that is exactly what is asserted here.

// The identity the whole scheme rests on: every dimension settles at its estimated per-hour rate
// times the hours actually observed. Not the reservation, not the wall clock between submission
// and termination — the observed hours alone.
func TestSettlementIsObservedHoursTimesTheJobsOwnRate(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 31, step, now) // 30 x 1min = 0.5 observed hours
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-identity",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-2 * time.Hour),
		EstimatedDurationHours: 2,
		EstimatedCostAccH:      10, // 5 AccH per hour
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	const observedHours = 0.5
	for _, d := range []struct {
		name      string
		key       domain.ResourceType
		estimated float64
	}{
		{"accelerator", domain.ResourceAcceleratorHours, 10},
	} {
		want := (d.estimated / exp.EstimatedDurationHours) * observedHours
		if got := usage.amounts[d.key]; !approx(got, want) {
			t.Errorf("%s settled at %v, want %v — the charge is not observed hours x the job's own rate", d.name, got, want)
		}
	}
}

// Settlement is an absolute set, recomputed from scratch every time, so running it repeatedly —
// which the reconciler, the job watcher and the cancel path all do, sometimes concurrently — must
// converge on one figure rather than accumulate. A delta-style write would double the charge here
// and this is the cheapest place to prove it does not.
func TestSettlingRepeatedlyDoesNotAccumulate(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 61, step, now) // 1 observed hour
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-idempotent",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-2 * time.Hour),
		EstimatedDurationHours: 2,
		EstimatedCostAccH:      10,
	}
	for i := 0; i < 3; i++ {
		if err := settler.Settle(context.Background(), exp); err != nil {
			t.Fatalf("Settle #%d: %v", i+1, err)
		}
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 5.0; !approx(got, want) {
		t.Errorf("after three settlements the charge is %v, want %v — settlement is accumulating instead of setting", got, want)
	}
}

// The over-billing case a preemption actually creates, and the one the e2e band was reaching for.
//
// A preempted job is requeued with its estimate rescaled to the work it has LEFT, in the same
// proportion across every dimension. That rescale must leave the job's per-hour RATE untouched,
// because settlement multiplies by cumulative lifetime hours: bill a resumed job at a rate derived
// from its shortened estimate and it pays twice for the stint it already ran.
//
// So the assertion is an equality between two settlements of the same observed hours — one for a
// job that ran straight through, one for the same job after a preemption rescaled it. A rescale
// that moved cost and duration by different factors shows up here as a difference, with no clock
// involved.
func TestAPreemptionRescaleDoesNotChangeWhatAnHourCosts(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 61, step, now) // 1 observed hour, for both jobs below
	defer server.Close()

	settle := func(exp *domain.Experiment) float64 {
		usage := &capturedUsage{}
		settler := New(usage, server.URL, 3*step)
		if err := settler.Settle(context.Background(), exp); err != nil {
			t.Fatalf("Settle %s: %v", exp.ID, err)
		}
		return usage.amounts[domain.ResourceAcceleratorHours]
	}

	uninterrupted := settle(&domain.Experiment{
		ID: "exp-straight-through", PlatformExperimentID: "pe-1",
		CreatedAt:              now.Add(-4 * time.Hour),
		EstimatedDurationHours: 4, EstimatedCostAccH: 20, // 5 AccH/h
	})

	// The same job, preempted a quarter of the way in: RequeuePreemptedPlan rescales duration and
	// every resource estimate by the same ratio (0.75), which is what keeps the rate at 5 AccH/h.
	rescaled := settle(&domain.Experiment{
		ID: "exp-preempted", PlatformExperimentID: "pe-1",
		CreatedAt:              now.Add(-4 * time.Hour),
		EstimatedDurationHours: 3, EstimatedCostAccH: 15,
		EvictionReason: string(domain.EvictionPreemptedForGuaranteed),
	})

	if !approx(uninterrupted, rescaled) {
		t.Fatalf("an observed hour cost %v before preemption and %v after — the requeue's rescale changed the rate, so a resumed job is billed twice for the stint it already ran",
			uninterrupted, rescaled)
	}
}
