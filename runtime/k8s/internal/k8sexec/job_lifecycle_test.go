package k8sexec

import (
	"context"
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadenv"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
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
	if grace != 5+workloadenv.LaunchKillMargin {
		t.Fatalf("delete grace = %d, want 5 and the launch margin: only a policy termination may spend the checkpoint window", grace)
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
	c := &JobWorkloadClient{apiURL: testAPIURL}
	exp := &clusterapi.DesiredJob{Image: testImage, Launch: "true",
		ID:              "grouped-placement",
		AcceleratorType: "unqualified-type",
		Job: clusterapi.JobSpec{
			MaxRetries: intPtr(0), TerminationGracePeriodSeconds: int64Ptr(5),
			AcceleratorType: "unqualified-type",
			Groups: []clusterapi.JobGroup{
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
	c := &JobWorkloadClient{apiURL: testAPIURL}
	exp := &clusterapi.DesiredJob{Image: testImage, Launch: "true",
		ID:              "cpu-only",
		AcceleratorType: "unqualified-type",
		Job: clusterapi.JobSpec{
			MaxRetries: intPtr(0), TerminationGracePeriodSeconds: int64Ptr(5),
			Groups: []clusterapi.JobGroup{
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

// Whether a job wants an accelerator is read from the job as admission resolved it, the one
// BuildJobs compiles and guards on, not from the job as it was submitted.
func TestPlacementIsResolvedForTheJobAdmissionResolved(t *testing.T) {
	c := &JobWorkloadClient{apiURL: testAPIURL}
	submitted := clusterapi.JobSpec{MaxRetries: intPtr(0), TerminationGracePeriodSeconds: int64Ptr(5),
		CPU: "1", Memory: "1Gi", Storage: "1Gi"}
	resolved := submitted
	resolved.AcceleratorType, resolved.AcceleratorCount = "unqualified-type", 1
	exp := &clusterapi.DesiredJob{Image: testImage, Launch: "true", ID: "resolved-placement", AcceleratorType: "unqualified-type", Job: submitted, ResolvedJob: &resolved}
	if _, err := c.resolvePlacementFor(context.Background(), exp); err == nil {
		t.Fatal("a job resolved to hold an accelerator was given the empty placement without any resolution attempt")
	}
}

// The namespace's policy lets no pod reach a job's, so a distributed job's pods reach each other
// only through a policy of the job's own, which admits its pods and no other, and goes with its
// headless Service.
func TestADistributedJobsPodsReachEachOtherAndNoOtherPod(t *testing.T) {
	ctx := context.Background()
	c := pullTestClient(t)
	exp := pullTestExperiment("exp-peers", testImage, nil)
	exp.Job.NumNodes = 2
	if err := c.CreateWorkload(ctx, exp); err != nil {
		t.Fatal(err)
	}
	policies := c.kube.NetworkingV1().NetworkPolicies(HypothesisLoopNamespace)
	policy, err := policies.Get(ctx, jobName(exp.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ownPods := map[string]string{workloadkeys.ExperimentID: exp.ID}
	if !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, ownPods) {
		t.Errorf("the policy selects %v, want the job's pods", policy.Spec.PodSelector.MatchLabels)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 || len(policy.Spec.Ingress[0].Ports) != 0 ||
		!reflect.DeepEqual(policy.Spec.Ingress[0].From[0], networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: ownPods}}) {
		t.Errorf("ingress = %+v, want from the job's own pods alone", policy.Spec.Ingress)
	}
	if len(policy.Spec.Egress) != 1 || !reflect.DeepEqual(policy.Spec.Egress[0].To, policy.Spec.Ingress[0].From) {
		t.Errorf("egress = %+v, want to the job's own pods alone", policy.Spec.Egress)
	}
	if owners := policy.OwnerReferences; len(owners) != 1 || owners[0].Kind != "Service" || owners[0].Name != jobName(exp.ID) {
		t.Errorf("owners = %+v, want the job's headless Service", owners)
	}
	if matches, err := c.WorkloadMatchesDesired(ctx, exp); err != nil || !matches {
		t.Fatalf("WorkloadMatchesDesired = %v, %v with the policy in place", matches, err)
	}
	if err := policies.Delete(ctx, policy.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if matches, err := c.WorkloadMatchesDesired(ctx, exp); err != nil || matches {
		t.Fatalf("WorkloadMatchesDesired = %v, %v with the policy gone, want a workload whose pods cannot reach each other replaced", matches, err)
	}

	single := pullTestExperiment("exp-single", testImage, nil)
	if err := c.CreateWorkload(ctx, single); err != nil {
		t.Fatal(err)
	}
	if _, err := policies.Get(ctx, jobName(single.ID), metav1.GetOptions{}); !errors.IsNotFound(err) {
		t.Fatalf("get the policy of a single-node job: %v, want none: it has no peers to reach", err)
	}
}
