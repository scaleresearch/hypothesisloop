package scheduler

import (
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/obsmetrics"
)

// backlogKey identifies one (cluster, flavor, tier) bucket of capacity-blocked demand —
// autoscaler.md's "Backlog signal for clusters without a native autoscaler" (secondary path).
// Only a signal for an *external* autoscaler (bare-metal power-on, cloud API script) to consume;
// clusters with a native autoscaler already get theirs from the Pending pod itself (the
// speculative-submit path), so this exists for the clusters that can't see that.
type backlogKey struct {
	cluster string
	flavor  string
	tier    string
}

// backlogAggregator collects one tick's capacity-blocked shortfall per bucket. Not persisted and
// not read back — a fresh Gauge snapshot published once at the end of every tick, same pattern as
// every other obsmetrics series in this package.
type backlogAggregator struct {
	unmet     map[backlogKey]int64
	oldestAge map[backlogKey]time.Duration
}

func newBacklogAggregator() *backlogAggregator {
	return &backlogAggregator{unmet: map[backlogKey]int64{}, oldestAge: map[backlogKey]time.Duration{}}
}

// record folds one skipped job's shortage into its (cluster, flavor, tier) bucket. cluster and
// exp.AcceleratorType may be "" (no candidate resolved at all) — still a real bucket: "this
// flavor has unmet demand with no cluster to even try."
func (b *backlogAggregator) record(now time.Time, cluster string, tier string, exp *domain.Experiment, shortage domain.Footprint) {
	key := backlogKey{
		cluster: cluster,
		flavor:  strings.ToLower(string(exp.AcceleratorType)),
		tier:    tier,
	}
	if n := acceleratorShortfall(shortage, exp.AcceleratorType); n > 0 {
		b.unmet[key] += n
	} else if _, ok := b.unmet[key]; !ok {
		b.unmet[key] = 0
	}
	if exp.QueuedAt != nil {
		age := now.Sub(*exp.QueuedAt)
		if age > b.oldestAge[key] {
			b.oldestAge[key] = age
		}
	}
}

// acceleratorShortfall reads the shortage vector in the job's own requested dimension — the same
// key negativeInDimension uses to read desired-free, so the two stay in the same units.
func acceleratorShortfall(shortage domain.Footprint, flavor domain.AcceleratorType) int64 {
	if shortage == nil || flavor == "" {
		return 0
	}
	key := domain.ResourceKey{Kind: domain.ResourceKindAccelerator, Flavor: strings.ToLower(string(flavor))}
	return shortage[key]
}

// publish subtracts each bucket's outstanding speculative footprint (already-being-served demand,
// per autoscaler.md line 114 — "the backlog gauge must subtract outstanding speculative jobs so
// demand already being served is not re-reported") and sets the gauges. Called once at the end of
// tick(); Reset() first so a bucket with no unmet demand this tick drops off the series instead of
// reporting a stale nonzero value forever.
func (b *backlogAggregator) publish(speculativeFootprintByCluster map[string]int) {
	obsmetrics.SchedulerUnmetDemand.Reset()
	obsmetrics.SchedulerUnmetDemandOldestWaitSeconds.Reset()
	for key, unmet := range b.unmet {
		served := int64(speculativeFootprintByCluster[key.cluster])
		remaining := unmet - served
		if remaining < 0 {
			remaining = 0
		}
		obsmetrics.SchedulerUnmetDemand.WithLabelValues(key.cluster, key.flavor, key.tier).Set(float64(remaining))
		obsmetrics.SchedulerUnmetDemandOldestWaitSeconds.WithLabelValues(key.cluster, key.flavor, key.tier).Set(b.oldestAge[key].Seconds())
	}
}
