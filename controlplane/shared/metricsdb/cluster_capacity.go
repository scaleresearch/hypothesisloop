package metricsdb

import (
	"context"
	"fmt"
	"time"
)

// clusterAcceleratorAvailableMetric/clusterAcceleratorTotalMetric hold cluster-agents' self-reported live
// per-flavor accelerator capacity (allocatable extended-resource quantity minus actually-requested,
// computed per accelerator flavor against a cluster's real node/pod state — see
// workload.GetLiveAcceleratorCapacity). Written on every desired-state poll, same as the CPU capacity
// this mirrors. Capacity is metric data like everything else on this client: it lives here,
// never duplicated into Postgres.
const (
	clusterAcceleratorAvailableMetric = "cluster_accelerator_available_accelerators"
	clusterAcceleratorTotalMetric     = "cluster_accelerator_total_accelerators"
)

// RecordClusterAcceleratorCapacity writes one gauge sample per (flavor, available/total) pair for
// clusterName, all in a single remote-write batch.
func RecordClusterAcceleratorCapacity(ctx context.Context, dbURL, clusterName string, availableByFlavor, totalByFlavor map[string]int64) error {
	if len(availableByFlavor) == 0 {
		return nil
	}
	now := time.Now().UTC()
	samples := make([]GaugeSample, 0, 2*len(availableByFlavor))
	for flavor, avail := range availableByFlavor {
		samples = append(samples, GaugeSample{
			MetricName: clusterAcceleratorAvailableMetric,
			Labels:     map[string]string{"cluster_name": clusterName, "flavor": flavor},
			Value:      float64(avail),
			At:         now,
		})
	}
	for flavor, total := range totalByFlavor {
		samples = append(samples, GaugeSample{
			MetricName: clusterAcceleratorTotalMetric,
			Labels:     map[string]string{"cluster_name": clusterName, "flavor": flavor},
			Value:      float64(total),
			At:         now,
		})
	}
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordClusterAcceleratorCapacity: %w", err)
	}
	return nil
}

// LiveClusterAcceleratorCapacity returns the most recently reported accelerator availability per cluster per
// flavor, restricted to samples within `window` of now — a cluster (or a single flavor on a
// cluster) with no report inside the window is simply absent from the result, exactly like
// metricsdb.IsAlive's freshness gating: a stale or disconnected cluster contributes nothing
// rather than a possibly-long-stale number.
func LiveClusterAcceleratorCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]int64, error) {
	promQL := fmt.Sprintf(`last_over_time(%s[%s])`, clusterAcceleratorAvailableMetric, promSeconds(window))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterAcceleratorCapacity: %w", err)
	}
	out := make(map[string]map[string]int64)
	for _, s := range samples {
		cluster := s.Labels["cluster_name"]
		flavor := s.Labels["flavor"]
		if cluster == "" || flavor == "" {
			continue
		}
		if out[cluster] == nil {
			out[cluster] = make(map[string]int64)
		}
		out[cluster][flavor] = int64(s.Value)
	}
	return out, nil
}

// clusterRAMAvailableMetric/clusterRAMTotalMetric and their storage equivalents hold
// cluster-agents' self-reported live RAM/ephemeral-storage capacity in bytes — the Class B
// (hard-cap, no billing) counterpart to CPU/Accelerator's capacity reporting above (see
// SCHEDULING_GENERALIZATION_PLAN.md's Class B step 2). Same "metric data, never duplicated into
// Postgres" rule applies.
const (
	clusterRAMAvailableMetric     = "cluster_ram_available_bytes"
	clusterRAMTotalMetric         = "cluster_ram_total_bytes"
	clusterStorageAvailableMetric = "cluster_storage_available_bytes"
	clusterStorageTotalMetric     = "cluster_storage_total_bytes"
)

// recordClusterScalarCapacity writes one gauge sample each for availMetric/totalMetric, scoped
// to clusterName — the shared plumbing behind RecordClusterRAMCapacity/RecordClusterStorageCapacity.
func recordClusterScalarCapacity(ctx context.Context, dbURL, clusterName, availMetric, totalMetric string, availableBytes, totalBytes int64) error {
	now := time.Now().UTC()
	samples := []GaugeSample{
		{MetricName: availMetric, Labels: map[string]string{"cluster_name": clusterName}, Value: float64(availableBytes), At: now},
		{MetricName: totalMetric, Labels: map[string]string{"cluster_name": clusterName}, Value: float64(totalBytes), At: now},
	}
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.recordClusterScalarCapacity(%s): %w", availMetric, err)
	}
	return nil
}

// liveClusterScalarCapacity returns the most recently reported value of availMetric per
// cluster, restricted to samples within window of now — see LiveClusterAcceleratorCapacity's doc
// comment for the staleness-gating rationale (a stale/disconnected cluster contributes nothing).
func liveClusterScalarCapacity(ctx context.Context, dbURL, availMetric string, window time.Duration) (map[string]int64, error) {
	promQL := fmt.Sprintf(`last_over_time(%s[%s])`, availMetric, promSeconds(window))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.liveClusterScalarCapacity(%s): %w", availMetric, err)
	}
	out := make(map[string]int64, len(samples))
	for _, s := range samples {
		if cluster := s.Labels["cluster_name"]; cluster != "" {
			out[cluster] = int64(s.Value)
		}
	}
	return out, nil
}

// RecordClusterRAMCapacity/LiveClusterRAMCapacity report and read live per-cluster RAM
// availability in bytes (allocatable minus requested, computed only against nodes an
// already-scheduled pod is actually assigned to — see workload.JobWorkloadClient.GetLiveRAMCapacity).
func RecordClusterRAMCapacity(ctx context.Context, dbURL, clusterName string, availableBytes, totalBytes int64) error {
	return recordClusterScalarCapacity(ctx, dbURL, clusterName, clusterRAMAvailableMetric, clusterRAMTotalMetric, availableBytes, totalBytes)
}

func LiveClusterRAMCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterRAMAvailableMetric, window)
}

// RecordClusterStorageCapacity/LiveClusterStorageCapacity are RAM's ephemeral-storage
// counterpart — same semantics, same accepted limitation (ephemeral-storage enforcement in
// Kubernetes is eviction-based, not a hard cgroup ceiling like memory — see
// SCHEDULING_GENERALIZATION_PLAN.md Class B step 4).
func RecordClusterStorageCapacity(ctx context.Context, dbURL, clusterName string, availableBytes, totalBytes int64) error {
	return recordClusterScalarCapacity(ctx, dbURL, clusterName, clusterStorageAvailableMetric, clusterStorageTotalMetric, availableBytes, totalBytes)
}

func LiveClusterStorageCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]int64, error) {
	return liveClusterScalarCapacity(ctx, dbURL, clusterStorageAvailableMetric, window)
}
