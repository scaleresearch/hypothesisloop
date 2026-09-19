// Package workloadenv is the environment both runtimes hand a workload, so a job reads the same
// variables wherever it was placed.
package workloadenv

import (
	"crypto/sha256"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

type Var struct{ Name, Value string }

// Platform returns the variables the platform sets on every node of group, in a fixed order: the
// k8s runtime hashes the pod spec built from them, and a reordering would read as drift on every
// running job. reach turns a URL of the platform into the one the workload reaches it at.
//
// The accelerator count is the group's per-node one, never the job total: a pod only holds its
// own devices, and the total made every pod of a 4x2 job size itself for 8.
//
// The attempt is load-bearing beyond telling the workload. A requeue reuses the experiment id, so
// without the attempt in desired state the rebuilt workload is identical to the terminal one
// still in the cluster, the reconciler reads that dead workload as matching desired state, and
// nothing is recreated.
func Platform(exp *domain.Experiment, group domain.JobGroup, apiURL string, reach func(url string) string) ([]Var, error) {
	// Durable data is desired state: the control plane computes the address and hands it over
	// with the credentials. A job that silently runs without a place to save is worse than one
	// that never starts.
	if exp.Data == nil {
		return nil, fmt.Errorf("experiment %s has no durable-data access in desired state", exp.ID)
	}
	dataEndpoint := reach(exp.Data.Endpoint)
	traceID := sha256.Sum256([]byte(exp.ID))
	return []Var{
		{"AWS_ACCESS_KEY_ID", exp.Data.AccessKeyID},
		{"AWS_ENDPOINT_URL", dataEndpoint},
		{"AWS_ENDPOINT_URL_S3", dataEndpoint},
		{"AWS_REGION", exp.Data.Region},
		{"AWS_SECRET_ACCESS_KEY", exp.Data.SecretAccessKey},
		{"HYPOTHESISLOOP_DATA_SHARED", exp.Data.Shared},
		{"HYPOTHESISLOOP_DATA_URI", exp.Data.URI},
		{"HYPOTHESISLOOP_EXPERIMENT_ID", exp.ID},
		{"HYPOTHESISLOOP_AGENT_ID", exp.AgentID},
		{"HYPOTHESISLOOP_PROJECT_ID", exp.ProjectID},
		{"HYPOTHESISLOOP_CODE_REF", exp.CodeRef},
		{"HYPOTHESISLOOP_CONFIG_HASH", exp.ConfigHash},
		{"HYPOTHESISLOOP_DATA_REF", exp.DataRef},
		{"HYPOTHESISLOOP_API_URL", reach(apiURL)},
		{"HYPOTHESISLOOP_ACCELERATOR_TYPE", string(exp.AcceleratorType)},
		{"HYPOTHESISLOOP_ACCELERATOR_COUNT", fmt.Sprintf("%d", group.AcceleratorCount)},
		{"HYPOTHESISLOOP_DURATION_SECONDS", fmt.Sprintf("%d", int(exp.EstimatedDurationHours*3600))},
		{"HYPOTHESISLOOP_ATTEMPT", fmt.Sprintf("%d", exp.AttemptCount)},
		{"TRACEPARENT", fmt.Sprintf("00-%x-%x-01", traceID[:16], traceID[16:24])},
	}, nil
}

// Job returns the job's own environment with the group's entries over it: the job-level map says
// what is true of every node, the group's what is true of its role.
func Job(spec domain.JobSpec, group domain.JobGroup) map[string]string {
	env := make(map[string]string, len(spec.Env)+len(group.Env))
	for k, v := range spec.Env {
		env[k] = v
	}
	for k, v := range group.Env {
		env[k] = v
	}
	return env
}
