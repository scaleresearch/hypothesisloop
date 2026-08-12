package k8sexec

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
