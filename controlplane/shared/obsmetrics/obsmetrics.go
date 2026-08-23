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
		StaleDesiredStateExperiments)
}
