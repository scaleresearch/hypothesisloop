package obsmetrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Eviction reasons carry a per-job detail by design (important.md #21) — the image that could not
// be pulled, the node, the numbers. That detail is exactly what must never become a Prometheus
// label: one time series per eviction eventually kills the scrape. This asserts the counter
// collapses to the code, so evictions differing only in detail land on one series.
func TestEvictionsWithDifferentDetailsShareOneSeries(t *testing.T) {
	EvictedExperimentsTotal.Reset()

	CountEviction(domain.EvictionUnschedulable.WithDetail(`Back-off pulling image "a:1"`))
	CountEviction(domain.EvictionUnschedulable.WithDetail(`Back-off pulling image "b:2"`))
	// A caller holding a bare constant must land on that same series, not a second one.
	CountEviction(domain.EvictionUnschedulable)

	series := collect(t)
	if len(series) != 1 {
		t.Fatalf("series = %d %v, want 1 — the detail is minting a time series per eviction", len(series), series)
	}
	if got := series["unschedulable"]; got != 3 {
		t.Errorf("unschedulable count = %v, want 3", got)
	}
}

// collect reads the counter's current series as label -> value.
func collect(t *testing.T) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	EvictedExperimentsTotal.Collect(ch)
	close(ch)
	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		for _, l := range pb.GetLabel() {
			if l.GetName() == "reason" {
				out[l.GetValue()] = pb.GetCounter().GetValue()
			}
		}
	}
	return out
}
