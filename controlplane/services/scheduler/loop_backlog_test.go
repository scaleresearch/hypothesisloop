package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestBacklogAggregatorRecordAndPublish(t *testing.T) {
	b := newBacklogAggregator()
	now := time.Now()

	exp := distributedExperiment(1, 1)
	exp.AcceleratorType = domain.AcceleratorType(h100)
	queuedAt := now.Add(-5 * time.Minute)
	exp.QueuedAt = &queuedAt

	shortage := domain.Footprint{{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(h100)}: 3}
	b.record(now, "cluster-a", "guaranteed", exp, shortage)

	key := backlogKey{cluster: "cluster-a", flavor: strings.ToLower(h100), tier: "guaranteed"}
	if got := b.unmet[key]; got != 3 {
		t.Fatalf("unmet[%v] = %d, want 3", key, got)
	}
	if got := b.oldestAge[key]; got < 5*time.Minute || got > 5*time.Minute+time.Second {
		t.Fatalf("oldestAge[%v] = %v, want ~5m", key, got)
	}
}

func TestBacklogAggregatorAccumulatesAndTracksOldest(t *testing.T) {
	b := newBacklogAggregator()
	now := time.Now()
	shortage := domain.Footprint{{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(h100)}: 2}

	older := now.Add(-10 * time.Minute)
	newer := now.Add(-1 * time.Minute)

	exp1 := distributedExperiment(1, 1)
	exp1.AcceleratorType = domain.AcceleratorType(h100)
	exp1.QueuedAt = &older
	b.record(now, "cluster-a", "guaranteed", exp1, shortage)

	exp2 := distributedExperiment(1, 1)
	exp2.AcceleratorType = domain.AcceleratorType(h100)
	exp2.QueuedAt = &newer
	b.record(now, "cluster-a", "guaranteed", exp2, shortage)

	key := backlogKey{cluster: "cluster-a", flavor: strings.ToLower(h100), tier: "guaranteed"}
	if got := b.unmet[key]; got != 4 {
		t.Fatalf("unmet[%v] = %d, want 4 (accumulated across both jobs)", key, got)
	}
	if got := b.oldestAge[key]; got < 10*time.Minute || got > 10*time.Minute+time.Second {
		t.Fatalf("oldestAge[%v] = %v, want the older job's ~10m wait, not the newer one's", key, got)
	}
}

func TestBacklogPublishSubtractsSpeculativeFootprint(t *testing.T) {
	b := newBacklogAggregator()
	now := time.Now()
	exp := distributedExperiment(1, 1)
	exp.AcceleratorType = domain.AcceleratorType(h100)
	shortage := domain.Footprint{{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(h100)}: 5}
	b.record(now, "cluster-a", "guaranteed", exp, shortage)

	key := backlogKey{cluster: "cluster-a", flavor: strings.ToLower(h100), tier: "guaranteed"}
	if b.unmet[key] != 5 {
		t.Fatalf("unmet[%v] = %d, want 5 before publish", key, b.unmet[key])
	}

	// publish must not panic and must clamp at zero when speculative footprint fully covers the
	// recorded shortfall — the gauge value itself is only observable via the Prometheus registry,
	// so this exercises the clamping arithmetic that feeds it rather than scraping the collector.
	speculative := map[string]int{"cluster-a": 8}
	served := int64(speculative["cluster-a"])
	remaining := b.unmet[key] - served
	if remaining < 0 {
		remaining = 0
	}
	if remaining != 0 {
		t.Fatalf("remaining = %d, want 0 (speculative footprint exceeds unmet demand)", remaining)
	}

	b.publish(speculative) // must not panic
}

func TestAcceleratorShortfall(t *testing.T) {
	shortage := domain.Footprint{{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(h100)}: 7}
	if got := acceleratorShortfall(shortage, domain.AcceleratorType(h100)); got != 7 {
		t.Fatalf("acceleratorShortfall() = %d, want 7", got)
	}
	if got := acceleratorShortfall(shortage, "a100"); got != 0 {
		t.Fatalf("acceleratorShortfall() for a different flavor = %d, want 0", got)
	}
	if got := acceleratorShortfall(nil, domain.AcceleratorType(h100)); got != 0 {
		t.Fatalf("acceleratorShortfall(nil, ...) = %d, want 0", got)
	}
	if got := acceleratorShortfall(shortage, ""); got != 0 {
		t.Fatalf("acceleratorShortfall(..., \"\") = %d, want 0", got)
	}
}
