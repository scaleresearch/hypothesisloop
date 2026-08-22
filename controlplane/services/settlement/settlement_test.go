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
