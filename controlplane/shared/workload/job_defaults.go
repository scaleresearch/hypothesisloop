package workload

import (
	"context"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// JobDefaults are the operator-owned, per-cluster fallback values merged into an agent's
// JobSpec for anything it left unset. They live in-cluster as a ConfigMap (see
// jobDefaultsConfigMapName below and cluster/infra/job-defaults-configmap.yaml) rather than
// in the cross-cluster control-plane config, because they are inherently a property of one
// specific cluster's execution engine (e.g. "this cluster has no accelerator nodes yet, so default
// every job's accelerator request to the 'cpu' resource instead") — exactly the kind of concern
// that belongs downlevel, behind the Backend seam, not in the agent-facing DSL.
type JobDefaults struct {
	CPU        string
	Memory     string
	Storage    string
	MaxRetries int
}

// defaultJobDefaults is the fallback used when no ConfigMap is present (fresh clusters,
// local/dev runs) so job submission still works out of the box.
func defaultJobDefaults() JobDefaults {
	return JobDefaults{CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: 3}
}

// jobDefaultsConfigMapName is the well-known in-cluster ConfigMap operators edit to tune
// this cluster's fallback resource shape, read fresh on every job build (no controller,
// no restart required to pick up a change — just `kubectl edit configmap` in-cluster).
const jobDefaultsConfigMapName = "openresearch-job-defaults"

// jobDefaults fetches this cluster's JobDefaults ConfigMap, falling back to
// defaultJobDefaults() if it doesn't exist or is malformed — a missing ConfigMap is not an
// error, just "use the built-in fallback."
func (c *JobWorkloadClient) jobDefaults(ctx context.Context) JobDefaults {
	def := defaultJobDefaults()
	cm, err := c.kube.CoreV1().ConfigMaps(OpenResearchNamespace).Get(ctx, jobDefaultsConfigMapName, metav1.GetOptions{})
	if err != nil {
		return def
	}
	if v, ok := cm.Data["cpu"]; ok && v != "" {
		def.CPU = v
	}
	if v, ok := cm.Data["memory"]; ok && v != "" {
		def.Memory = v
	}
	if v, ok := cm.Data["storage"]; ok && v != "" {
		def.Storage = v
	}
	if v, ok := cm.Data["max_retries"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			def.MaxRetries = n
		}
	}
	return def
}

// mergeJobDefaults fills every unset field of an agent's JobSpec from this cluster's
// JobDefaults — the merge step that lets agents omit anything they don't care about
// instead of restating the operator's own cluster-shape decisions.
func mergeJobDefaults(job domain.JobSpec, def JobDefaults) domain.JobSpec {
	if job.CPU == "" {
		job.CPU = def.CPU
	}
	if job.Memory == "" {
		job.Memory = def.Memory
	}
	if job.Storage == "" {
		job.Storage = def.Storage
	}
	if job.MaxRetries == nil {
		job.MaxRetries = &def.MaxRetries
	}
	return job
}
