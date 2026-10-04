package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

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

func TestTickPublishesBacklogWhenNoBurstJobIsQueued(t *testing.T) {
	obsmetrics.SchedulerUnmetDemand.Reset()
	blocked := queuedJob("exp-a", "pe-live", domain.CapacityGuaranteed)
	store := &resilienceStore{queued: []*domain.Experiment{blocked}}

	if err := resilienceLoop(store).tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	var m dto.Metric
	gauge := obsmetrics.SchedulerUnmetDemand.WithLabelValues("c1", strings.ToLower(string(blocked.AcceleratorType)), "guaranteed")
	if err := gauge.Write(&m); err != nil {
		t.Fatal(err)
	}
	if got := m.GetGauge().GetValue(); got != 1 {
		t.Fatalf("unmet demand gauge = %v, want 1 for the blocked guaranteed job", got)
	}
}
