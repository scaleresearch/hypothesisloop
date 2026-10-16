// Package workloadenv is the environment and the command every runtime hands a workload, so a job
// reads the same variables and runs the same process wherever it was placed.
package workloadenv

import (
	"crypto/sha256"
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
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
func Platform(exp *clusterapi.DesiredJob, group clusterapi.JobGroup, apiURL string, reach func(url string) string) ([]Var, error) {
	// Durable data is desired state: the control plane computes the address and hands it over
	// with the credentials. A job that silently runs without a place to save is worse than one
	// that never starts.
	if exp.Data == nil {
		return nil, fmt.Errorf("experiment %s has no durable-data access in desired state", exp.ID)
	}
	if exp.Token == "" {
		return nil, fmt.Errorf("experiment %s has no job token in desired state", exp.ID)
	}
	if exp.GitURL == "" {
		return nil, fmt.Errorf("experiment %s has no repository address in desired state", exp.ID)
	}
	cpus, err := allottedCPUs(group.CPU)
	if err != nil {
		return nil, err
	}
	dataEndpoint := reach(exp.Data.Endpoint)
	traceID := sha256.Sum256([]byte(exp.ID))
	vars := []Var{
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
	}
	if cpus != "" {
		// A container sees every core of its node (nproc, os.cpu_count) while its cpu limit lets
		// it use only these, so a thread pool sized from the node is throttled.
		vars = append(vars, Var{"HYPOTHESISLOOP_CPUS", cpus}, Var{"OMP_NUM_THREADS", cpus})
	}
	return append(vars,
		Var{"TRACEPARENT", fmt.Sprintf("00-%x-%x-01", traceID[:16], traceID[16:24])},
		Var{"HYPOTHESISLOOP_TOKEN", exp.Token},
		Var{"HYPOTHESISLOOP_GIT_URL", reach(exp.GitURL)},
	), nil
}

// allottedCPUs is a node's cpu quantity as whole cores, rounded up, or empty when none is stated.
func allottedCPUs(cpu string) (string, error) {
	if cpu == "" {
		return "", nil
	}
	q, err := resource.ParseQuantity(cpu)
	if err != nil {
		return "", fmt.Errorf("job cpu %q: %w", cpu, err)
	}
	return strconv.FormatInt(max((q.MilliValue()+999)/1000, 1), 10), nil
}

// Job returns the job's own environment with the group's entries over it: the job-level map says
// what is true of every node, the group's what is true of its role. The job's secret_env is part
// of it; admission refused a name the job or a group also sets in the open.
func Job(spec clusterapi.JobSpec, group clusterapi.JobGroup) map[string]string {
	env := make(map[string]string, len(spec.Env)+len(group.Env)+len(spec.SecretEnv))
	for k, v := range spec.Env {
		env[k] = v
	}
	for k, v := range group.Env {
		env[k] = v
	}
	for k, v := range spec.SecretEnv {
		env[k] = v
	}
	return env
}

// Merged is the whole environment of one node of group as one map: the job's own entries, and the
// platform's for every name the job did not set itself.
func Merged(exp *clusterapi.DesiredJob, spec clusterapi.JobSpec, group clusterapi.JobGroup, apiURL string, reach func(url string) string) (map[string]string, error) {
	platform, err := Platform(exp, group, apiURL, reach)
	if err != nil {
		return nil, err
	}
	env := Job(spec, group)
	for _, v := range platform {
		if _, set := env[v.Name]; !set {
			env[v.Name] = v.Value
		}
	}
	return env, nil
}

// Command is the process one node of group runs: the group's own when it names one, the job's
// shared entrypoint otherwise. An ungrouped job's one group carries the job-level values.
func Command(spec clusterapi.JobSpec, group clusterapi.JobGroup) (command, args []string) {
	if len(group.Command) > 0 {
		return group.Command, group.Args
	}
	return spec.Command, spec.Args
}
