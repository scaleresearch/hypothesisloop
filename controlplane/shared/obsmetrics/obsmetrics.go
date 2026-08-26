// Package obsmetrics is the admission/eviction-loop self-observability that Kueue and Volcano
// both have and we didn't.
// Registered on the default Prometheus registry, so it's served by the existing /metrics
// endpoint (promhttp.Handler()) with no additional wiring per service.
package obsmetrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

var (
	// EvictedExperimentsTotal counts evictions by reason — mirrors Kueue's
	// EvictedWorkloadsTotal{reason,...}. Same call site as the existing eviction-reason DB
	// write (controller.evict, job_watcher.onStuckPending), no new data source needed.
	EvictedExperimentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hypothesisloop_evicted_experiments_total",
		Help: "Total experiments evicted, labeled by reason.",
	}, []string{"reason"})

	// AdmissionTickDuration times one full Loop.tick() pass — answers "is the admission loop
	// getting slower."
	AdmissionTickDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "hypothesisloop_admission_tick_duration_seconds",
		Help:    "Duration of one scheduler admission loop tick.",
		Buckets: prometheus.DefBuckets,
	})

	// AdmissionTickResultsTotal counts per-tick outcomes (admitted/skipped/preempted) per
	// capacity tier — answers "how many jobs were considered vs skipped this tick."
	AdmissionTickResultsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hypothesisloop_admission_tick_results_total",
		Help: "Admission loop outcomes, labeled by tier and result.",
	}, []string{"tier", "result"})

	// StaleDesiredStateExperiments is a point-in-time count from the last GC sweep pass — an
	// orphaned SUBMITTED/ADMITTED/RUNNING experiment with no recent actual-state phase metric.
	// Alert-only: this never triggers a state change, just visibility for operator review.
	StaleDesiredStateExperiments = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hypothesisloop_stale_desired_state_experiments",
		Help: "Count of SUBMITTED/ADMITTED/RUNNING experiments with no recent cluster job report, from the last GC sweep pass.",
	})

	// SpeculativeSubmitsTotal counts every SUBMITTED row created against a cluster with no live
	// fit — autoscaler.md's speculative-submit path (loop_speculate.go). Answers "how often are we
	// asking a native autoscaler to boot a node."
	SpeculativeSubmitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hypothesisloop_speculative_submits_total",
		Help: "Total speculative (no-live-fit) submits to autoscaler-enabled clusters.",
	})

	// FailoversTotal counts every time a speculative attempt is abandoned and the cluster is
	// appended to the job's tried_clusters list — labeled by why (autoscaler.md's
	// scale_up_timeout / flavor_mismatch paths). A steady failover rate on one cluster is the
	// signal the design doc's "fan-out is not adopted, revisit if failover is common" call
	// depends on.
	FailoversTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hypothesisloop_failovers_total",
		Help: "Total speculative-submit failovers (cluster added to a job's tried_clusters), labeled by reason.",
	}, []string{"reason"})

	// SchedulerUnmetDemand is autoscaler.md's backlog signal for clusters with no native
	// autoscaler to react to a Pending pod: capacity-blocked shortfall per (cluster, flavor,
	// tier), minus outstanding speculative footprint already being served. Feeds an external
	// autoscaler (bare-metal power-on, cloud API script) — secondary path, nothing in the
	// control plane consumes it.
	SchedulerUnmetDemand = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hypothesisloop_scheduler_unmet_demand",
		Help: "Capacity-blocked accelerator shortfall per cluster/flavor/tier, net of outstanding speculative submits.",
	}, []string{"cluster", "flavor", "tier"})

	// SchedulerUnmetDemandOldestWaitSeconds is the same bucketing as SchedulerUnmetDemand, but
	// the longest a still-blocked job in that bucket has been queued — how urgently an external
	// autoscaler should react, not just whether it should.
	SchedulerUnmetDemandOldestWaitSeconds = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hypothesisloop_scheduler_unmet_demand_oldest_wait_seconds",
		Help: "Age of the longest-waiting still-blocked job per cluster/flavor/tier bucket.",
	}, []string{"cluster", "flavor", "tier"})
)

// CountEviction is the only way to increment EvictedExperimentsTotal. Reasons carry a per-job
// detail in production (domain.EvictionReason.WithDetail — the image that could not be pulled, the
// node, the numbers), and labelling with that detail mints a time series per eviction, which
// eventually kills the scrape. Code() folds it back to the constant.
func CountEviction(reason domain.EvictionReason) {
	EvictedExperimentsTotal.WithLabelValues(string(reason.Code())).Inc()
}

func init() {
	prometheus.MustRegister(EvictedExperimentsTotal, AdmissionTickDuration, AdmissionTickResultsTotal,
		StaleDesiredStateExperiments, SpeculativeSubmitsTotal, FailoversTotal,
		SchedulerUnmetDemand, SchedulerUnmetDemandOldestWaitSeconds)
}
