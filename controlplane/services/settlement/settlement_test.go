package settlement

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// aliveServer answers ObserveSpan with a job observed continuously for (points-1) x step,
// ending at now — what a healthy running job looks like. Kept in "boundary points" rather than a
// bare duration so the fixtures below still read as "n samples, one per step".
func aliveServer(t *testing.T, points int, step time.Duration, now time.Time) *httptest.Server {
	t.Helper()
	observed := time.Duration(points-1) * step
	if points < 1 {
		observed = 0
	}
	first := now.Add(-observed)
	body := fmt.Sprintf(`{"output":[{"records":{"rows":[[%d,%d,%d,%d]]}}]}`,
		first.UnixMilli(), now.UnixMilli(), observed.Milliseconds(), observed.Milliseconds())
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

type capturedUsage struct {
	amounts map[domain.ResourceType]float64
}

func (c *capturedUsage) SetObservedUsage(_ context.Context, _ *domain.Experiment, amounts map[domain.ResourceType]float64) error {
	c.amounts = amounts
	return nil
}

// A job that outruns its estimate settles at its estimated per-hour rate times the hours it
// really ran — no ceiling at the reservation. Billing the reservation instead would write a
// number into the metrics store that never happened; the bound on overrun is the controller's
// quota-exhaustion check evicting it, not settlement under-reporting it.
func TestSettleBillsOverrunAtTheEstimatedRateWithNoCeiling(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 121, step, now) // 121 boundaries = 120 x 1min intervals = 2 observed hours
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-overrun",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-3 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		EstimatedCPUCoreHours:  4,
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 16.0; !approx(got, want) {
		t.Errorf("accelerator settled at %v, want %v (2h at 8 AccH/h)", got, want)
	}
	if got, want := usage.amounts[domain.ResourceCPUCoreHours], 8.0; !approx(got, want) {
		t.Errorf("cpu settled at %v, want %v (2h at 4 core-h/h)", got, want)
	}
	// A dimension nothing was reserved on has no series to settle.
	if _, ok := usage.amounts[domain.ResourceRAMGBHours]; ok {
		t.Errorf("ram settled despite no reservation: %v", usage.amounts)
	}
}

// A job that never posted an observation consumed nothing, whatever it reserved and however it
// was terminated.
func TestSettleChargesNothingForAJobThatNeverRan(t *testing.T) {
	server := httptest.NewServer(emptyObservationsHandler())
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*time.Minute)

	exp := &domain.Experiment{
		ID:                     "exp-never-ran",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              time.Now().UTC().Add(-time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := usage.amounts[domain.ResourceAcceleratorHours]; got != 0 {
		t.Errorf("accelerator settled at %v, want 0", got)
	}
}

func emptyObservationsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":[{"records":{"rows":[[null,null,null,null]]}}]}`))
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// An infrastructure fault refunds every hour the job burned. It ran, but the environment is what
// ended it, so the agent never chose to spend those hours — and a system whose whole output is a
// ranking of agents cannot charge one for a broken node. The refund is expressed as zero observed
// hours through this same absolute write rather than as a separate credit: a second write path to
// one figure is free to disagree with this one and to double-apply across the retries Settle
// exists to be safe under.
func TestAJobThatEndedEvictedOnAnInfrastructureFaultSettlesToAFullRefund(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 121, step, now) // two genuinely observed hours
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-bad-node",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-3 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		EstimatedCPUCoreHours:  4,
		Status:                 domain.StatusEvicted,
		EvictionReason:         string(domain.EvictionClusterUnreachable),
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 0.0; !approx(got, want) {
		t.Errorf("accelerator settled at got = %v, want %v", got, want)
	}
	if got, want := usage.amounts[domain.ResourceCPUCoreHours], 0.0; !approx(got, want) {
		t.Errorf("cpu settled at got = %v, want %v", got, want)
	}
}

// A workload fault is the agent's own, so it pays for exactly what it ran. Refunding here would
// make every failure free and remove the only cost signal an agent has for its own bugs.
func TestAWorkloadFaultIsBilledForEveryHourItRan(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 121, step, now)
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-hung",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-3 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		Status:                 domain.StatusEvicted,
		EvictionReason:         string(domain.EvictionSilent),
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 16.0; !approx(got, want) {
		t.Errorf("accelerator settled at got = %v, want %v", got, want)
	}
}

// A policy termination is the platform's own decision, not a fault, and it is reported separately
// for that reason — but the researcher still genuinely ran the hours, so a stage cut bills like
// any other outcome. Only infrastructure changes the figure.
func TestAPolicyTerminationIsBilledLikeAnyOtherOutcome(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 121, step, now)
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-cut",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-3 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		Status:                 domain.StatusEvicted,
		EvictionReason:         string(domain.EvictionStageCut),
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 16.0; !approx(got, want) {
		t.Errorf("accelerator settled at got = %v, want %v", got, want)
	}
}

// The reason carries per-job detail through EvictionReason.WithDetail, and classification must be
// driven by the typed code rather than the message text — otherwise a detailed reason silently
// falls out of its class and gets billed as if it were the agent's fault.
func TestAnInfrastructureFaultCarryingDetailIsStillRefunded(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 121, step, now)
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-detailed",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-3 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		Status:                 domain.StatusEvicted,
		EvictionReason:         string(domain.EvictionWorkloadGone.WithDetail("no pod on cluster tt-small")),
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 0.0; !approx(got, want) {
		t.Errorf("accelerator settled at got = %v, want %v", got, want)
	}
}

// The bug this test exists for: eviction_reason deliberately survives an infrastructure requeue
// as the record of what ended the previous attempt, so a job requeued once and then successful
// reaches COMPLETED still carrying `cluster_unreachable`. Keying the refund on the reason alone
// settled that whole job — the successful attempt included — at zero, which handed an agent an
// unmetered run for the price of provoking one infrastructure fault. The refund must key on how
// the job actually ENDED.
func TestACompletedJobCarryingAStaleInfrastructureReasonIsStillBilled(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 121, step, now)
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-requeued-then-completed",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-3 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		Status:                 domain.StatusCompleted,
		EvictionReason:         string(domain.EvictionClusterUnreachable),
		InfraRequeueCount:      1,
		AttemptCount:           1,
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 16.0; !approx(got, want) {
		t.Errorf("accelerator settled at got = %v, want %v", got, want)
	}
}

// The same stale reason on the other terminal outcome. A job requeued for an infrastructure fault
// and then failing on its own — a bug in the workload — is the agent's failure and must be billed
// like one; the reason it happens to still carry says nothing about how this attempt ended.
func TestAFailedJobCarryingAStaleInfrastructureReasonIsStillBilled(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 121, step, now)
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-requeued-then-failed",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-3 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		Status:                 domain.StatusFailed,
		EvictionReason:         string(domain.EvictionWorkloadGone),
		InfraRequeueCount:      1,
		AttemptCount:           1,
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 16.0; !approx(got, want) {
		t.Errorf("accelerator settled at got = %v, want %v", got, want)
	}
}

// A job still QUEUED after an infrastructure requeue is mid-flight, not ended. Settling it to zero
// would make a job cycling through requeues invisible to running-cost and to the controller's
// quota-exhaustion check, which read this same figure every tick.
func TestAnInfrastructureRequeuedJobBackInTheQueueIsBilledForWhatItHasBurned(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	server := aliveServer(t, 121, step, now)
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-requeued",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-3 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		Status:                 domain.StatusQueued,
		EvictionReason:         string(domain.EvictionClusterUnreachable),
		InfraRequeueCount:      1,
		AttemptCount:           1,
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 16.0; !approx(got, want) {
		t.Errorf("accelerator settled at got = %v, want %v", got, want)
	}
}
