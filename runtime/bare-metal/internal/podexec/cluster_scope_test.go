package podexec

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

func jobContainer(cluster, state string, scoped bool) *fakeContainer {
	labels := map[string]string{
		LabelManagedBy: ManagedByValue, LabelCPUCores: "1000", LabelMemoryBytes: "1024", LabelStorageBytes: "1024",
		LabelGraceSeconds: "0", LabelCheckpointGrace: "0", LabelAttempt: "0", LabelAcceleratorCnt: "0",
	}
	if scoped {
		labels[LabelCluster] = cluster
	}
	return &fakeContainer{labels: labels, state: state}
}

// One engine can serve the agents of several clusters: two bare-metal agents on a machine, or
// the tests of a shared development host. An agent that managed every container carrying the
// platform's label stopped the other cluster's jobs as no longer desired.
func TestAnAgentManagesOnlyTheContainersOfItsOwnCluster(t *testing.T) {
	engine := newFakeEngine()
	for id, c := range map[string]*fakeContainer{
		"mine": jobContainer("east", "running", true), "mine-done": jobContainer("east", "exited", true),
		"theirs": jobContainer("west", "running", true), "from-before": jobContainer("", "running", false),
	} {
		c.labels[LabelExperimentID] = id
		engine.containers[containerName(id)] = c
	}
	east := newFakeExecutor(engine)
	east.clusterName = "east"
	ctx := context.Background()

	running, err := east.ListManagedJobs(ctx)
	if err != nil || !slices.Equal(running, []string{"mine"}) {
		t.Fatalf("running jobs = %v, err = %v; want only this cluster's", running, err)
	}
	reported, err := east.ListManagedJobsForStatus(ctx)
	slices.Sort(reported)
	if err != nil || !slices.Equal(reported, []string{"mine", "mine-done"}) {
		t.Fatalf("jobs reported = %v, err = %v; want only this cluster's", reported, err)
	}
	requests, err := east.sumRunningRequests(ctx)
	if err != nil || requests.cpuMilli != 1000 {
		t.Fatalf("CPU counted as in use = %dm, err = %v; want this cluster's one running job", requests.cpuMilli, err)
	}

	// Nothing is desired, so everything this agent holds goes, and nothing else.
	for _, id := range []string{"mine", "theirs", "from-before"} {
		if err := east.DeleteWorkload(ctx, id, false); err != nil {
			t.Fatal(err)
		}
	}
	east.Wait()
	if err := east.ReapTerminal(ctx, nil); err != nil {
		t.Fatal(err)
	}
	east.Wait()
	engine.mu.Lock()
	if stopped := engine.stops[containerName("theirs")] + engine.stops[containerName("from-before")]; stopped != 0 {
		t.Errorf("a container of another cluster or from before was stopped")
	}
	var left []string
	for name := range engine.containers {
		left = append(left, strings.TrimPrefix(name, containerNamePrefix))
	}
	engine.mu.Unlock()
	slices.Sort(left)
	if !slices.Equal(left, []string{"from-before", "theirs"}) {
		t.Fatalf("containers left on the engine = %v, want the other cluster's and the one from before untouched", left)
	}
}

func TestAContainerCarriesTheClusterThatStartedIt(t *testing.T) {
	e := &Executor{apiURL: "http://example.invalid:8083", clusterName: "east"}
	cs, err := e.BuildContainerSpec(sizedExperiment(), Placement{})
	if err != nil {
		t.Fatal(err)
	}
	if cs.Labels[LabelCluster] != "east" {
		t.Fatalf("labels = %v, want the cluster named", cs.Labels)
	}
}

// An exited container the engine reports no state for read as exit code 0, a job that succeeded.
func TestAnExitedContainerWithNoReportedStateIsFailedWithAReason(t *testing.T) {
	engine := newFakeEngine()
	c := jobContainer("east", "exited", true)
	c.labels[LabelExperimentID] = "unknown-end"
	c.stateless = true
	engine.containers[containerName("unknown-end")] = c
	east := newFakeExecutor(engine)
	east.clusterName = "east"

	status, err := east.PollJobStatus(context.Background(), "unknown-end")
	if err != nil || status.Phase != clusterapi.JobPhaseFailed {
		t.Fatalf("status = %s (err %v), want failed: nothing says the command succeeded", status.Phase, err)
	}
	reason, message, _, _, _, err := east.PollPhaseDetail(context.Background(), "unknown-end", 0)
	if err != nil || reason != clusterapi.PhaseReasonContainerFailed || !strings.Contains(message, "no state") {
		t.Fatalf("phase detail = %q %q (err %v), want the failure and that the engine reported no state", reason, message, err)
	}
}

// Every state the engine has is reported as what it is. A paused container read as pending with
// nothing said, a dead one as pending for good, and so did any state nobody had thought of.
func TestEveryEngineStateIsReportedByNameAndAnUnknownOneIsAFailureWithTheReason(t *testing.T) {
	for state, want := range map[string]struct {
		phase   clusterapi.JobPhase
		reason  string
		message string
	}{
		"running":     {clusterapi.JobPhaseRunning, "", ""},
		"created":     {clusterapi.JobPhasePending, "", ""},
		"restarting":  {clusterapi.JobPhasePending, "", ""},
		"paused":      {clusterapi.JobPhasePending, "", "paused"},
		"removing":    {clusterapi.JobPhaseGone, "", ""},
		"dead":        {clusterapi.JobPhaseFailed, clusterapi.PhaseReasonContainerFailed, "dead"},
		"hibernating": {clusterapi.JobPhaseFailed, clusterapi.PhaseReasonContainerFailed, `"hibernating"`},
	} {
		t.Run(state, func(t *testing.T) {
			engine := newFakeEngine()
			c := jobContainer("east", state, true)
			c.labels[LabelExperimentID] = "job"
			engine.containers[containerName("job")] = c
			east := newFakeExecutor(engine)
			east.clusterName = "east"

			status, err := east.PollJobStatus(context.Background(), "job")
			if err != nil || status.Phase != want.phase {
				t.Fatalf("status = %s (err %v), want %s", status.Phase, err, want.phase)
			}
			reason, message, _, _, _, err := east.PollPhaseDetail(context.Background(), "job", 0)
			if err != nil || reason != want.reason || !strings.Contains(message, want.message) || (want.message == "" && message != "") {
				t.Fatalf("phase detail = %q %q (err %v), want reason %q and a message with %q", reason, message, err, want.reason, want.message)
			}
		})
	}
}

func TestAContainerThatExitedNonZeroIsReportedAsFailedWithItsCode(t *testing.T) {
	engine := newFakeEngine()
	for id, exit := range map[string]int{"crashed": 3, "finished": 0} {
		c := jobContainer("east", "exited", true)
		c.labels[LabelExperimentID] = id
		c.exit = exit
		engine.containers[containerName(id)] = c
	}
	east := newFakeExecutor(engine)
	east.clusterName = "east"

	reason, message, _, _, _, err := east.PollPhaseDetail(context.Background(), "crashed", 0)
	if err != nil || reason != clusterapi.PhaseReasonContainerFailed || message != "container exited with code 3" {
		t.Fatalf("phase detail = %q %q (err %v), want the failure and its exit code", reason, message, err)
	}
	if reason, message, _, _, _, err := east.PollPhaseDetail(context.Background(), "finished", 0); err != nil || reason != "" || message != "" {
		t.Fatalf("phase detail of a clean exit = %q %q (err %v), want none", reason, message, err)
	}
}
