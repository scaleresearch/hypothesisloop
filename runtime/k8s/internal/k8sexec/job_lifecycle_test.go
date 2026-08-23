package k8sexec

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExistingJobMatchesDesiredRequiresHashAndIdentity(t *testing.T) {
	desired := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{DesiredSpecHashAnnotation: "wanted"}}}
	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Labels:      map[string]string{"hypothesisloop.io/managed-by": "hypothesisloop", "hypothesisloop.io/experiment-id": "exp-1"},
		Annotations: map[string]string{DesiredSpecHashAnnotation: "stale"},
	}}
	if existingJobMatchesDesired(existing, desired, "exp-1") {
		t.Fatal("mismatched desired hash was accepted")
	}
	existing.Annotations[DesiredSpecHashAnnotation] = "wanted"
	delete(existing.Labels, "hypothesisloop.io/experiment-id")
	if existingJobMatchesDesired(existing, desired, "exp-1") {
		t.Fatal("missing experiment identity was accepted")
	}
}

// A policy-class termination -- preemption, a stage cut, quota exhaustion, the duration cap --
// is the platform's own decision about a job that was doing nothing wrong, so it is signalled
// and then given the window it declared. Deleting it on the ordinary shutdown grace is what made
// preemption lose the whole run: the scheduler's requeue already rescales the estimate to the
// hours left, on the assumption the job resumes where it stopped.
func TestAPolicyTerminationGivesThePodTheCheckpointWindowItDeclared(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "exp-1-0", Annotations: map[string]string{GraceSecondsAnnotation: "5"}},
		Spec:       corev1.PodSpec{TerminationGracePeriodSeconds: int64Ptr(300)},
	}
	grace, err := podDeleteGrace(pod, true)
	if err != nil {
		t.Fatal(err)
	}
	if grace != 300 {
		t.Fatalf("delete grace = %d, want 300: a job the platform chose to stop must get its checkpoint window", grace)
	}
}

// An infrastructure or workload failure gets no window: there is nothing to save, or nothing
// left to save it with. Handing one the same window would hold contended accelerators for
// minutes on a job whose node is already gone or whose process has already died.
func TestANonPolicyTerminationGivesThePodNoCheckpointWindow(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "exp-1-0", Annotations: map[string]string{GraceSecondsAnnotation: "5"}},
		Spec:       corev1.PodSpec{TerminationGracePeriodSeconds: int64Ptr(300)},
	}
	grace, err := podDeleteGrace(pod, false)
	if err != nil {
		t.Fatal(err)
	}
	if grace != 5 {
		t.Fatalf("delete grace = %d, want 5: only a policy termination may spend the checkpoint window", grace)
	}
}

// A pod whose shutdown grace cannot be read is an error, not a guessed number. The guess would
// only ever be wrong for a job that declared a checkpoint window -- exactly the case where
// getting it wrong silently costs hardware or costs the job its work.
func TestAPodWithNoReadableShutdownGraceIsAnErrorRatherThanAGuess(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "exp-1-0"},
		Spec:       corev1.PodSpec{TerminationGracePeriodSeconds: int64Ptr(300)},
	}
	if _, err := podDeleteGrace(pod, false); err == nil {
		t.Fatal("a pod with no recorded shutdown grace was silently assigned one")
	}
}
