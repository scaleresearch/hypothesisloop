package podexec

import (
	"bytes"
	"context"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
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
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

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

	// A container from before containers named their cluster is nobody's to stop on a guess. It
	// is said once, however many passes list it.
	if said := strings.Count(logged.String(), "from before containers named their cluster"); said != 1 {
		t.Fatalf("the container from before was reported %d times:\n%s", said, logged.String())
	}
	if !strings.Contains(logged.String(), containerName("from-before")) {
		t.Fatalf("the report does not name the container:\n%s", logged.String())
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
	if err != nil || reason != domain.PhaseReasonContainerFailed || message != "container exited with code 3" {
		t.Fatalf("phase detail = %q %q (err %v), want the failure and its exit code", reason, message, err)
	}
	if reason, message, _, _, _, err := east.PollPhaseDetail(context.Background(), "finished", 0); err != nil || reason != "" || message != "" {
		t.Fatalf("phase detail of a clean exit = %q %q (err %v), want none", reason, message, err)
	}
}
