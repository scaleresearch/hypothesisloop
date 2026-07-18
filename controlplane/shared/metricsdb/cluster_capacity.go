package metricsdb

import (
	"context"
	"fmt"
	"time"
)

// clusterGPUAvailableMetric/clusterGPUTotalMetric hold cluster-agents' self-reported live
// per-flavor GPU capacity (allocatable extended-resource quantity minus actually-requested,
// computed per GPU flavor against a cluster's real node/pod state — see
// workload.GetLiveGPUCapacity). Written on every desired-state poll, same as the CPU capacity
// this mirrors. Capacity is metric data like everything else on this client: it lives here,
// never duplicated into Postgres.
const (
	clusterGPUAvailableMetric = "cluster_gpu_available_gpus"
	clusterGPUTotalMetric     = "cluster_gpu_total_gpus"
)

// RecordClusterGPUCapacity writes one gauge sample per (flavor, available/total) pair for
// clusterName, all in a single remote-write batch.
func RecordClusterGPUCapacity(ctx context.Context, dbURL, clusterName string, availableByFlavor, totalByFlavor map[string]int64) error {
	if len(availableByFlavor) == 0 {
		return nil
	}
	now := time.Now().UTC()
	samples := make([]GaugeSample, 0, 2*len(availableByFlavor))
	for flavor, avail := range availableByFlavor {
		samples = append(samples, GaugeSample{
			MetricName: clusterGPUAvailableMetric,
			Labels:     map[string]string{"cluster_name": clusterName, "flavor": flavor},
			Value:      float64(avail),
			At:         now,
		})
	}
	for flavor, total := range totalByFlavor {
		samples = append(samples, GaugeSample{
			MetricName: clusterGPUTotalMetric,
			Labels:     map[string]string{"cluster_name": clusterName, "flavor": flavor},
			Value:      float64(total),
			At:         now,
		})
	}
	if err := WriteGaugesAt(ctx, dbURL, samples); err != nil {
		return fmt.Errorf("metricsdb.RecordClusterGPUCapacity: %w", err)
	}
	return nil
}

// LiveClusterGPUCapacity returns the most recently reported GPU availability per cluster per
// flavor, restricted to samples within `window` of now — a cluster (or a single flavor on a
// cluster) with no report inside the window is simply absent from the result, exactly like
// metricsdb.IsAlive's freshness gating: a stale or disconnected cluster contributes nothing
// rather than a possibly-long-stale number.
func LiveClusterGPUCapacity(ctx context.Context, dbURL string, window time.Duration) (map[string]map[string]int64, error) {
	promQL := fmt.Sprintf(`last_over_time(%s[%s])`, clusterGPUAvailableMetric, promSeconds(window))
	samples, err := QueryVector(ctx, dbURL, promQL)
	if err != nil {
		return nil, fmt.Errorf("metricsdb.LiveClusterGPUCapacity: %w", err)
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
