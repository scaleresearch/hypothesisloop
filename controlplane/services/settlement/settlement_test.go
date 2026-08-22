package settlement

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v1/sql") {
			_, _ = w.Write([]byte(body))
			return
		}
		// Nothing settled yet — the prior-settlement floor is empty.
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
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
	server := httptest.NewServer(emptyObservationsHandler(""))
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

// Sample retention is finite, so for an old enough job "no observation in the window" stops
// meaning "never ran", and it degrades gradually rather than all at once. Settling is idempotent
// and re-runs long after the fact, so every dimension is floored at what was already settled for
// it: a re-settlement reading a shrunken window used to refund a job that genuinely ran.
func TestSettleNeverReducesAnAlreadyBilledJobAsItsSamplesAgeOut(t *testing.T) {
	// Observations aged out (empty range query), but the earlier settlement is still on record.
	server := httptest.NewServer(emptyObservationsHandler(`{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"resource_type":"accelerator_hours"},"value":[0,"16"]}]}}`))
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*time.Minute)

	exp := &domain.Experiment{
		ID:                     "exp-long-finished",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              time.Now().UTC().Add(-90 * 24 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 16.0; !approx(got, want) {
		t.Errorf("re-settlement wrote %v after the samples aged out, want the settled %v held", got, want)
	}
}

// emptyObservationsHandler serves a metrics store holding no observations for the experiment:
// ObserveSpan finds nothing. instantResult is what the already-settled usage lookup returns.
func emptyObservationsHandler(instantResult string) http.HandlerFunc {
	if instantResult == "" {
		instantResult = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v1/sql") {
			_, _ = w.Write([]byte(`{"output":[{"records":{"rows":[[null,null,null,null]]}}]}`))
			return
		}
		_, _ = w.Write([]byte(instantResult))
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

// Retention degrades gradually: a re-settlement can see a shorter span rather than none at all.
// The recomputed figure is still positive, just smaller, which the never-refund rule keyed on an
// empty window would have waved through.
func TestSettleHoldsThePriorFigureWhenTheWindowShrinks(t *testing.T) {
	step := time.Minute
	now := time.Now().UTC()
	// Only 30 minutes of the run survive retention; 2 hours were settled at the time.
	server := shrunkenObservationServer(t, 30*step, now, `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"resource_type":"accelerator_hours"},"value":[0,"16"]},
		{"metric":{"resource_type":"cpu_core_hours"},"value":[0,"8"]}]}}`)
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*step)

	exp := &domain.Experiment{
		ID:                     "exp-aging",
		PlatformExperimentID:   "pe-1",
		CreatedAt:              now.Add(-30 * 24 * time.Hour),
		EstimatedDurationHours: 1,
		EstimatedCostAccH:      8,
		EstimatedCPUCoreHours:  4,
	}
	if err := settler.Settle(context.Background(), exp); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, want := usage.amounts[domain.ResourceAcceleratorHours], 16.0; !approx(got, want) {
		t.Errorf("accelerator re-settled at %v, want the prior %v held", got, want)
	}
	if got, want := usage.amounts[domain.ResourceCPUCoreHours], 8.0; !approx(got, want) {
		t.Errorf("cpu re-settled at %v, want the prior %v held", got, want)
	}
}

// shrunkenObservationServer answers ObserveSpan with `observed` of surviving run time and the
// instant query with an earlier, larger settlement.
func shrunkenObservationServer(t *testing.T, observed time.Duration, now time.Time, settled string) *httptest.Server {
	t.Helper()
	first := now.Add(-observed)
	span := fmt.Sprintf(`{"output":[{"records":{"rows":[[%d,%d,%d,%d]]}}]}`,
		first.UnixMilli(), now.UnixMilli(), observed.Milliseconds(), observed.Milliseconds())
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/v1/sql") {
			_, _ = w.Write([]byte(span))
			return
		}
		_, _ = w.Write([]byte(settled))
	}))
}
