package k8sexec

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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

// A grouped job states its accelerators per group and leaves the top-level accelerator_count at
// zero -- the spec REJECTS a top-level count alongside groups. Keying "does this job want an
// accelerator?" on that top-level number therefore skipped placement resolution for every
// grouped job, handed BuildJobs the empty placement, and failed it at the guard that reads the
// group totals: the job was admitted, held its reservation, and never compiled into a single
// Job object. Resolution has to be attempted, which here surfaces as the type's own validation
// error rather than a silent empty placement.
func TestAGroupedJobResolvesPlacementFromItsGroupsNotItsEmptyTopLevelCount(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		ID:              "grouped-placement",
		AcceleratorType: "unqualified-type",
		Job: domain.JobSpec{
			Image: "example.invalid/workload", MaxRetries: intPtr(0),
			AcceleratorType: "unqualified-type",
			Groups: []domain.JobGroup{
				{Name: "learner", Replicas: 1, CPU: "2", Memory: "8Gi", Storage: "5Gi", AcceleratorCount: 1},
				{Name: "actor", Replicas: 2, CPU: "1", Memory: "1Gi", Storage: "1Gi"},
			},
		},
	}
	if _, err := c.resolvePlacementFor(context.Background(), exp); err == nil {
		t.Fatal("a grouped job asking for an accelerator was given the empty placement without any resolution attempt — it can never be compiled into a Job")
	}
}

// The other half of the same rule: a job that genuinely asks for no accelerator must still take
// the zero placement without touching the cluster, so an ordinary CPU job neither reads DRA
// inventory nor fails on an accelerator type it never named.
func TestAJobWithNoAcceleratorsInAnyGroupTakesTheZeroPlacement(t *testing.T) {
	c := &JobWorkloadClient{apiURL: APIURLDefault}
	exp := &domain.Experiment{
		ID:              "cpu-only",
		AcceleratorType: "unqualified-type",
		Job: domain.JobSpec{
			Image: "example.invalid/workload", MaxRetries: intPtr(0),
			Groups: []domain.JobGroup{
				{Name: "worker", Replicas: 2, CPU: "1", Memory: "1Gi", Storage: "1Gi"},
			},
		},
	}
	placement, err := c.resolvePlacementFor(context.Background(), exp)
	if err != nil {
		t.Fatal(err)
	}
	if placement.ResourceName != "" || placement.DeviceClassName != "" {
		t.Fatalf("placement = %+v, want the zero placement: a job requesting no accelerator must not be placed on one", placement)
	}
}
