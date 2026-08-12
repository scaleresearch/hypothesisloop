package k8sexec

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func pod(name string, ageMinutes int, exitCode *int32) corev1.Pod {
	p := corev1.Pod{}
	p.Name = name
	p.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Duration(ageMinutes) * time.Minute))
	if exitCode != nil {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: *exitCode}},
		}}
	}
	return p
}

func i32(v int32) *int32 { return &v }

// The retry case that lost every traceback: attempt 1 died with a Python error, attempt 2 is a
// fresh pod whose log is just a startup banner. Reporting only the newest pod discarded the
// error, so both must be selected.
func TestSelectLogPodsKeepsFailedAttemptAlongsideRetry(t *testing.T) {
	items := []corev1.Pod{
		pod("attempt-1", 10, i32(1)),
		pod("attempt-2", 2, nil),
	}
	newest, failed := selectLogPods(items)
	if newest == nil || newest.Name != "attempt-2" {
		t.Errorf("newest = %v, want attempt-2", newest)
	}
	if failed == nil || failed.Name != "attempt-1" {
		t.Errorf("failed = %v, want attempt-1", failed)
	}
}

func TestSelectLogPodsPicksMostRecentFailure(t *testing.T) {
	items := []corev1.Pod{
		pod("attempt-1", 30, i32(1)),
		pod("attempt-2", 20, i32(2)),
		pod("attempt-3", 1, nil),
	}
	_, failed := selectLogPods(items)
	if failed == nil || failed.Name != "attempt-2" {
		t.Errorf("failed = %v, want the most recent failure attempt-2", failed)
	}
}

// A healthy single-pod job must behave exactly as before: current output, no failure section.
func TestSelectLogPodsHealthyJobHasNoFailedPod(t *testing.T) {
	newest, failed := selectLogPods([]corev1.Pod{pod("only", 5, nil)})
	if newest == nil || newest.Name != "only" {
		t.Errorf("newest = %v, want only", newest)
	}
	if failed != nil {
		t.Errorf("failed = %v, want none for a job that has not failed", failed)
	}
}

// A clean exit is not a failure — exit 0 must not produce a "failed attempt" section.
func TestSelectLogPodsIgnoresSuccessfulTermination(t *testing.T) {
	if _, failed := selectLogPods([]corev1.Pod{pod("done", 5, i32(0))}); failed != nil {
		t.Errorf("failed = %v, want none for exit code 0", failed)
	}
}

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
	reason, message, restarts, err := terminatedPhaseDetail(
		&corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}, 2)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if reason != domain.PhaseReasonContainerFailed {
		t.Errorf("reason = %q, want %q", reason, domain.PhaseReasonContainerFailed)
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
	reason, message, _, _ := terminatedPhaseDetail(
		&corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}, 0)
	if reason != domain.PhaseReasonOOMKilled {
		t.Errorf("reason = %q, want %q", reason, domain.PhaseReasonOOMKilled)
	}
	if !strings.Contains(message, "137") {
		t.Errorf("message = %q, want the exit code", message)
	}
}

// Reporting a failure must not by itself condemn a job that still has retries left.
func TestContainerFailureDoesNotForceEarlyEviction(t *testing.T) {
	for _, r := range []string{domain.PhaseReasonContainerFailed, domain.PhaseReasonOOMKilled} {
		if domain.NeverSelfHealsPhaseReasons[r] {
			t.Errorf("%s must not be in NeverSelfHealsPhaseReasons — it would evict before retries run", r)
		}
	}
}

func restartedPod(name string, restarts int32, lastExit int32) corev1.Pod {
	p := pod(name, 5, nil)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		RestartCount: restarts,
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: lastExit},
		},
	}}
	return p
}

// RestartPolicy=OnFailure restarts the container in place, so GetLogs serves the replacement's
// startup banner and the traceback is only reachable via Previous. This is the case that
// destroyed a hypothesis: restart_count=1, 21 lines of startup, crash output gone.
func TestPodHasRestartedDetectsInPlaceRestart(t *testing.T) {
	p := restartedPod("only-pod", 1, 1)
	if !podHasRestarted(&p) {
		t.Error("a container with restart_count=1 must be treated as having an earlier instance")
	}
	if got := lastTerminationExitCode(&p); got != 1 {
		t.Errorf("lastTerminationExitCode = %d, want 1", got)
	}
}

// A pod that never restarted must not trigger a Previous read — there is no earlier instance
// and the request would just error.
func TestPodHasRestartedFalseForFirstInstance(t *testing.T) {
	p := pod("fresh", 1, nil)
	if podHasRestarted(&p) {
		t.Error("a pod on its first instance has no previous logs to fetch")
	}
	if got := lastTerminationExitCode(&p); got != 0 {
		t.Errorf("lastTerminationExitCode = %d, want 0", got)
	}
}

// The in-place restart and the multi-pod retry are independent mechanisms; a pod can be both
// the newest and have restarted, and both sections must be reachable.
func TestRestartedNewestPodIsStillSelectedAsNewest(t *testing.T) {
	items := []corev1.Pod{restartedPod("attempt-1", 1, 1)}
	newest, failed := selectLogPods(items)
	if newest == nil || newest.Name != "attempt-1" {
		t.Fatalf("newest = %v, want attempt-1", newest)
	}
	if failed != nil {
		t.Error("an in-place restart is not a terminated pod — it must not be reported as a failed attempt")
	}
	if !podHasRestarted(newest) {
		t.Error("newest pod's earlier instance must still be recognised")
	}
}
