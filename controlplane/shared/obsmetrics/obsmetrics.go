// Package obsmetrics is the admission/eviction-loop self-observability that Kueue and Volcano
// both have and we didn't (see items #17/#18 in competetors/SYNTHESIS_GAPS_AND_PLAN.md).
// Registered on the default Prometheus registry, so it's served by the existing /metrics
// endpoint (promhttp.Handler()) with no additional wiring per service.
package obsmetrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

var (
	// EvictedExperimentsTotal counts evictions by reason — mirrors Kueue's
	// EvictedWorkloadsTotal{reason,...}. Same call site as the existing eviction-reason DB
	// write (controller.evict, job_watcher.onStuckPending), no new data source needed.
	EvictedExperimentsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openresearch_evicted_experiments_total",
		Help: "Total experiments evicted, labeled by reason.",
	}, []string{"reason"})

	// AdmissionTickDuration times one full Loop.tick() pass — answers "is the admission loop
	// getting slower."
	AdmissionTickDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "openresearch_admission_tick_duration_seconds",
		Help:    "Duration of one scheduler admission loop tick.",
		Buckets: prometheus.DefBuckets,
	})

	// AdmissionTickResultsTotal counts per-tick outcomes (admitted/skipped/preempted) per
	// capacity tier — answers "how many jobs were considered vs skipped this tick."
	AdmissionTickResultsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openresearch_admission_tick_results_total",
		Help: "Admission loop outcomes, labeled by tier and result.",
	}, []string{"tier", "result"})

	// JobUIDMismatchTotal counts status reports whose Job UID didn't match the one first
	// observed for that experiment — an ownership-verification anomaly (see
	// competetors/SYNTHESIS_GAPS_AND_PLAN.md item #5), not expected in normal operation.
	JobUIDMismatchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "openresearch_job_uid_mismatch_total",
		Help: "Status reports whose Job UID didn't match the first-observed UID for that experiment, labeled by cluster.",
	}, []string{"cluster"})

	// StaleDesiredStateExperiments is a point-in-time count from the last GC sweep pass — an
	// orphaned SUBMITTED/ADMITTED/RUNNING experiment with no recent cluster_job_reports row.
	// Alert-only: this never triggers a state change, just visibility for operator review.
	StaleDesiredStateExperiments = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "openresearch_stale_desired_state_experiments",
		Help: "Count of SUBMITTED/ADMITTED/RUNNING experiments with no recent cluster job report, from the last GC sweep pass.",
	})
)

func init() {
	prometheus.MustRegister(EvictedExperimentsTotal, AdmissionTickDuration, AdmissionTickResultsTotal,
		JobUIDMismatchTotal, StaleDesiredStateExperiments)
}
