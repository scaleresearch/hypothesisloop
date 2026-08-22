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

// aliveServer answers every PromQL range query with the same matrix: `points` alive samples,
// one per `step`, ending at now. Both alive metrics (heartbeat and experiment_metric_value)
// resolve to the same grid, which is what a healthy running job looks like.
//
// Note what `points` buys: samples are boundaries, so n of them delimit n-1 intervals of `step`.
// A fixture meant to represent H hours of running therefore needs H/step + 1 points.
func aliveServer(t *testing.T, points int, step time.Duration, now time.Time) *httptest.Server {
	t.Helper()
	var values []string
	for i := points; i > 0; i-- {
		ts := now.Add(-time.Duration(i) * step).Unix()
		values = append(values, fmt.Sprintf(`[%d,"1"]`, ts))
	}
	body := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[%s]}]}}`,
		strings.Join(values, ","))
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
	settler := New(usage, server.URL, 3*step, step)

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
	settler := New(usage, server.URL, 3*time.Minute, time.Minute)

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
// meaning "never ran". Settling is idempotent and re-runs long after the fact, and a re-settlement
// that read the window as empty used to overwrite a real bill with zero — a full refund for a job
// that genuinely consumed hours.
func TestSettleDoesNotRefundAnAlreadyBilledJobOnceItsSamplesAgeOut(t *testing.T) {
	// Observations aged out (empty range query), but the earlier settlement is still on record.
	server := httptest.NewServer(emptyObservationsHandler(`{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"resource_type":"accelerator_hours"},"value":[0,"16"]}]}}`))
	defer server.Close()

	usage := &capturedUsage{}
	settler := New(usage, server.URL, 3*time.Minute, time.Minute)

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
	if usage.amounts != nil {
		t.Errorf("re-settlement overwrote a real bill after its samples aged out: %v", usage.amounts)
	}
}

// emptyObservationsHandler serves a metrics store holding no observations for the experiment.
// instantResult is what instant (vector) queries return — the already-settled usage lookup — while
// range queries always come back empty, which is what an aged-out or never-written series looks like.
func emptyObservationsHandler(instantResult string) http.HandlerFunc {
	if instantResult == "" {
		instantResult = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "query_range") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
			return
		}
		_, _ = w.Write([]byte(instantResult))
	}
}

func approx(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
