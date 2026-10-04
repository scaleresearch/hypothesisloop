package k8sexec

import (
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// The order of a pod's environment is part of the pod spec the reconciler hashes. A change of
// order, with every name and value the same, reads as drift on every running job and recreates
// all of them. These lists are that order: a variable is added at the end of its block, and a
// change to anything above it is a change to every running job.
var (
	platformEnvOrder = []string{
		"AWS_ACCESS_KEY_ID", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "AWS_REGION", "AWS_SECRET_ACCESS_KEY",
		"HYPOTHESISLOOP_DATA_SHARED", "HYPOTHESISLOOP_DATA_URI",
		"HYPOTHESISLOOP_EXPERIMENT_ID", "HYPOTHESISLOOP_AGENT_ID", "HYPOTHESISLOOP_PROJECT_ID",
		"HYPOTHESISLOOP_CODE_REF", "HYPOTHESISLOOP_CONFIG_HASH", "HYPOTHESISLOOP_DATA_REF", "HYPOTHESISLOOP_API_URL",
		"HYPOTHESISLOOP_ACCELERATOR_TYPE", "HYPOTHESISLOOP_ACCELERATOR_COUNT", "HYPOTHESISLOOP_DURATION_SECONDS",
		"HYPOTHESISLOOP_ATTEMPT", "HYPOTHESISLOOP_CPUS", "OMP_NUM_THREADS", "TRACEPARENT", "HYPOTHESISLOOP_TOKEN",
	}
	distributedEnvOrder = []string{
		"HYPOTHESISLOOP_WORLD_SIZE", "HYPOTHESISLOOP_MASTER_ADDR", "HYPOTHESISLOOP_MASTER_PORT",
		"LOCAL_RANK", "WORLD_SIZE", "MASTER_ADDR", "MASTER_PORT",
	}
	ungroupedRankEnvOrder = []string{"HYPOTHESISLOOP_RANK", "RANK"}
	groupedRankEnvOrder   = []string{"HYPOTHESISLOOP_GROUP", "HYPOTHESISLOOP_GROUP_RANK", "HYPOTHESISLOOP_GROUP_REPLICAS", "HYPOTHESISLOOP_RANK_OFFSET"}
)

func envNames(t *testing.T, exp *domain.Experiment) [][]string {
	t.Helper()
	c := &JobWorkloadClient{apiURL: testAPIURL}
	jobs, err := c.BuildJobs(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, job := range jobs {
		var names []string
		for _, env := range job.Spec.Template.Spec.Containers[0].Env {
			names = append(names, env.Name)
		}
		out = append(out, names)
	}
	return out
}

func joined(blocks ...[]string) []string {
	var out []string
	for _, block := range blocks {
		out = append(out, block...)
	}
	return out
}

func TestAPodsEnvironmentIsInAFixedOrder(t *testing.T) {
	// The job's own variables follow the platform's, sorted by name, the group's among them.
	jobEnv := map[string]string{"ZETA": "1", "ALPHA": "2", "Mid": "3"}
	ownEnvOrder := []string{"ALPHA", "Mid", "ZETA"}

	single := distributedExperiment(1, 0)
	single.Job.Env = jobEnv
	if got, want := envNames(t, single), [][]string{joined(platformEnvOrder, ownEnvOrder)}; !reflect.DeepEqual(got, want) {
		t.Errorf("one node:\n got %q\nwant %q", got, want)
	}

	distributed := distributedExperiment(3, 0)
	distributed.Job.Env = jobEnv
	if got, want := envNames(t, distributed), [][]string{joined(platformEnvOrder, ownEnvOrder, distributedEnvOrder, ungroupedRankEnvOrder)}; !reflect.DeepEqual(got, want) {
		t.Errorf("three nodes:\n got %q\nwant %q", got, want)
	}

	grouped := groupedExperiment()
	grouped.Job.Env = jobEnv
	grouped.Job.Groups[1].Env = map[string]string{"BETA": "4"}
	want := [][]string{
		joined(platformEnvOrder, ownEnvOrder, distributedEnvOrder, groupedRankEnvOrder),
		joined(platformEnvOrder, []string{"ALPHA", "BETA", "Mid", "ZETA"}, distributedEnvOrder, groupedRankEnvOrder),
	}
	if got := envNames(t, grouped); !reflect.DeepEqual(got, want) {
		t.Errorf("grouped:\n got %q\nwant %q", got, want)
	}
}
