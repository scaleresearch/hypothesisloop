package k8sexec

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/ranktail"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

func TestTerminatedExitCodeReportsWorstContainer(t *testing.T) {
	p := corev1.Pod{}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}},
	}
	if got := terminatedExitCode(&p); got != 137 {
		t.Errorf("terminatedExitCode = %d, want 137", got)
	}
}

// A FAILED experiment used to carry no failure information at all: the exit code and
// Kubernetes' termination Reason were discarded, and Terminated.Message is empty unless the
// workload writes to terminationMessagePath, which a crashing Python process does not.
func TestTerminatedPhaseDetailReportsExitCode(t *testing.T) {
	reason, message, restarts := terminatedPhaseDetail(
		&corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}, 2)
	if reason != clusterapi.PhaseReasonContainerFailed {
		t.Errorf("reason = %q, want %q", reason, clusterapi.PhaseReasonContainerFailed)
	}
	if !strings.Contains(message, "code 1") || !strings.Contains(message, "Error") {
		t.Errorf("message = %q, want it to name the exit code and reason", message)
	}
	if restarts != 2 {
		t.Errorf("restarts = %d, want 2", restarts)
	}
}

// OOM is called out separately because the fix differs — raise the request, not the code.
func TestTerminatedPhaseDetailDistinguishesOOM(t *testing.T) {
	reason, message, _ := terminatedPhaseDetail(
		&corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}, 0)
	if reason != clusterapi.PhaseReasonOOMKilled {
		t.Errorf("reason = %q, want %q", reason, clusterapi.PhaseReasonOOMKilled)
	}
	if !strings.Contains(message, "137") {
		t.Errorf("message = %q, want the exit code", message)
	}
}

// gangPod is one rank of a distributed job as BuildJobs compiles it: its completion index, and
// for a grouped job its group and that group's rank offset.
func gangPod(name string, index int, group string, offset int, node string) corev1.Pod {
	p := corev1.Pod{}
	p.Name, p.Namespace = name, HypothesisLoopNamespace
	p.Labels = map[string]string{workloadkeys.ExperimentID: "gang"}
	p.Annotations = map[string]string{
		batchv1.JobCompletionIndexAnnotation: strconv.Itoa(index),
		AcceleratorTypeAnnotation:            "type-a",
	}
	p.Spec.Subdomain, p.Spec.NodeName = "exp-gang", node
	var env []corev1.EnvVar
	if group != "" {
		env = []corev1.EnvVar{{Name: jobenv.Group, Value: group}, {Name: jobenv.RankOffset, Value: strconv.Itoa(offset)}}
	}
	p.Spec.Containers = []corev1.Container{{Name: "experiment", Env: env}}
	return p
}

func failedAt(p corev1.Pod, exit int32, at time.Time) corev1.Pod {
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, FinishedAt: metav1.NewTime(at)},
	}}}
	return p
}

func sectionRanks(sections []ranktail.Section) []int {
	ranks := make([]int, 0, len(sections))
	for _, s := range sections {
		ranks = append(ranks, s.Rank)
	}
	return ranks
}

func pickedSections(t *testing.T, pods []corev1.Pod, shown int) ([]ranktail.Section, int) {
	t.Helper()
	sections, err := (&JobWorkloadClient{}).rankSections(context.Background(), "gang", pods)
	if err != nil {
		t.Fatal(err)
	}
	return ranktail.Pick(sections, shown)
}

// Every reader of a tail reads its end, so the rank that explains the job comes last: the first
// rank to fail, the ranks that died after it before it, rank 0 before them and the rest first.
// A grouped job's rank is its group's offset plus its index, so the actors' index 0 is rank 1.
func TestRankSectionsPutTheFirstFailureLastAndRankZeroBeforeTheFailures(t *testing.T) {
	t0 := time.Now()
	pods := []corev1.Pod{
		failedAt(gangPod("actor-2", 2, "actor", 1, "n3"), 1, t0.Add(time.Second)),
		gangPod("actor-0", 0, "actor", 1, "n1"),
		failedAt(gangPod("actor-1", 1, "actor", 1, "n2"), 137, t0),
		gangPod("learner-0", 0, "learner", 0, "n0"),
	}
	sections, omitted := pickedSections(t, pods, 10)
	if got, want := sectionRanks(sections), []int{1, 0, 3, 2}; !reflect.DeepEqual(got, want) || omitted != 0 {
		t.Fatalf("ranks = %v with %d omitted, want %v with none", got, omitted, want)
	}
	if got, want := sections[3].Header(), "--- rank 2 failed, exit 137 (group actor, pod actor-1) ---"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
	if got, want := sections[1].Header(), "--- rank 0 (group learner, pod learner-0) ---"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
}

// A gang larger than the budget shows the failed ranks and rank 0 before any other rank.
func TestRankSectionsKeepFailuresAndRankZeroWhenNotEveryRankFits(t *testing.T) {
	var pods []corev1.Pod
	for i := 11; i >= 0; i-- {
		pods = append(pods, gangPod(fmt.Sprintf("exp-gang-%d", i), i, "", 0, "n"))
	}
	pods[0] = failedAt(pods[0], 1, time.Now())
	sections, omitted := pickedSections(t, pods, 3)
	if got, want := sectionRanks(sections), []int{1, 0, 11}; !reflect.DeepEqual(got, want) || omitted != 9 {
		t.Fatalf("ranks = %v with %d omitted, want %v with 9", got, omitted, want)
	}
	if got, want := sections[0].Header(), "--- rank 1 (pod exp-gang-1) ---"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
}

// A distributed job's tail is read rank by rank through the pods' own logs.
func TestFetchLogTailOfAGangHasOneSectionPerRank(t *testing.T) {
	pods := []corev1.Pod{gangPod("exp-gang-1", 1, "", 0, "n1"), gangPod("exp-gang-0", 0, "", 0, "n0")}
	c := pullTestClient(t, &pods[0], &pods[1])
	lines, err := c.FetchLogTail(context.Background(), "gang", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--- rank 1 (pod exp-gang-1) ---", "fake logs", "--- rank 0 (pod exp-gang-0) ---", "fake logs"}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("tail = %q, want %q", lines, want)
	}
}

// A gang pod with no completion index has no rank to report under, and saying so beats a guess.
func TestAGangPodWithoutACompletionIndexIsAnError(t *testing.T) {
	p := gangPod("exp-gang-0", 0, "", 0, "n0")
	delete(p.Annotations, batchv1.JobCompletionIndexAnnotation)
	if _, _, err := podGangRank(&p); err == nil {
		t.Fatal("a gang pod without a completion index was given a rank")
	}
}

// A single-node job's tail is its pod's own output, with no section headers.
func TestFetchLogTailOfASingleNodeJobIsItsPodsOutput(t *testing.T) {
	p := corev1.Pod{}
	p.Name, p.Namespace = "exp-single", HypothesisLoopNamespace
	p.Labels = map[string]string{workloadkeys.ExperimentID: "single"}
	c := pullTestClient(t, &p)
	lines, err := c.FetchLogTail(context.Background(), "single", 100)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"fake logs"}; !reflect.DeepEqual(lines, want) {
		t.Errorf("tail = %q, want %q", lines, want)
	}
}
