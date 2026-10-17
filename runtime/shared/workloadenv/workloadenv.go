// Package workloadenv is the environment and the command every runtime hands a workload, so a job
// reads the same variables and runs the same process wherever it was placed.
package workloadenv

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
)

// LaunchKillMargin is how much longer than a job's stop grace its runtime waits after SIGTERM
// before killing it, so the launch script's own kill at jobenv.StopGrace lands first and says why.
const LaunchKillMargin int64 = 5

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
	if exp.Image == "" || exp.Launch == "" {
		return nil, fmt.Errorf("experiment %s has no image or launch script in desired state", exp.ID)
	}
	cpus, err := allottedCPUs(group.CPU)
	if err != nil {
		return nil, err
	}
	shutdown, checkpoint, err := exp.EffectiveJob().ResolvedGrace()
	if err != nil {
		return nil, fmt.Errorf("experiment %s: %w", exp.ID, err)
	}
	dataEndpoint := reach(exp.Data.Endpoint)
	traceID := sha256.Sum256([]byte(exp.ID))
	vars := []Var{
		{jobenv.AccessKeyID, exp.Data.AccessKeyID},
		{jobenv.Endpoint, dataEndpoint},
		{jobenv.EndpointS3, dataEndpoint},
		{jobenv.Region, exp.Data.Region},
		{jobenv.SecretAccessKey, exp.Data.SecretAccessKey},
		{jobenv.DataShared, exp.Data.Shared},
		{jobenv.DataURI, exp.Data.URI},
		{jobenv.ExperimentID, exp.ID},
		{jobenv.AgentID, exp.AgentID},
		{jobenv.ProjectID, exp.ProjectID},
		{jobenv.CodeRef, exp.CodeRef},
		{jobenv.ConfigHash, exp.ConfigHash},
		{jobenv.DataRef, exp.DataRef},
		{jobenv.APIURL, reach(apiURL)},
		{jobenv.AcceleratorType, string(exp.AcceleratorType)},
		{jobenv.AcceleratorCount, fmt.Sprintf("%d", group.AcceleratorCount)},
		{jobenv.DurationSeconds, fmt.Sprintf("%d", int(exp.EstimatedDurationHours*3600))},
		{jobenv.Attempt, fmt.Sprintf("%d", exp.AttemptCount)},
	}
	if cpus != "" {
		// A container sees every core of its node (nproc, os.cpu_count) while its cpu limit lets
		// it use only these, so a thread pool sized from the node is throttled.
		vars = append(vars, Var{jobenv.CPUs, cpus}, Var{jobenv.OMPNumThreads, cpus})
	}
	return append(vars,
		Var{jobenv.TraceParent, fmt.Sprintf("00-%x-%x-01", traceID[:16], traceID[16:24])},
		Var{jobenv.Token, exp.Token},
		Var{jobenv.GitURL, reach(exp.GitURL)},
		Var{jobenv.StopGrace, strconv.FormatInt(max(shutdown, checkpoint), 10)},
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

// Vars is the whole environment of one node of group: the platform's variables in Platform's
// order, then the job's own sorted by name. A name the platform sets is the platform's whatever
// the job states, since the launch script and the platform read them; admission refuses a job
// that states one (jobenv.Reserved).
func Vars(exp *clusterapi.DesiredJob, spec clusterapi.JobSpec, group clusterapi.JobGroup, apiURL string, reach func(url string) string) ([]Var, error) {
	vars, err := Platform(exp, group, apiURL, reach)
	if err != nil {
		return nil, err
	}
	job := Job(spec, group)
	for _, v := range vars {
		delete(job, v.Name)
	}
	for _, name := range slices.Sorted(maps.Keys(job)) {
		vars = append(vars, Var{name, job[name]})
	}
	return vars, nil
}

// Merged is Vars as one map.
func Merged(exp *clusterapi.DesiredJob, spec clusterapi.JobSpec, group clusterapi.JobGroup, apiURL string, reach func(url string) string) (map[string]string, error) {
	vars, err := Vars(exp, spec, group, apiURL, reach)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, len(vars))
	for _, v := range vars {
		env[v.Name] = v.Value
	}
	return env, nil
}

// Command is the process one node of group runs: the job's launch script around the group's own
// command when it names one, the job's shared one otherwise. An ungrouped job's one group carries
// the job-level values.
func Command(exp *clusterapi.DesiredJob, spec clusterapi.JobSpec, group clusterapi.JobGroup) []string {
	if len(group.Command) > 0 {
		return clusterapi.LaunchCommand(exp.Launch, group.Command, group.Args)
	}
	return clusterapi.LaunchCommand(exp.Launch, spec.Command, spec.Args)
}
