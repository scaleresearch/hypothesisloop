package k8sexec

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

func attemptTestExperiment(attempt int) *clusterapi.DesiredJob {
	return &clusterapi.DesiredJob{Image: testImage, Launch: "true",
		Data: testDataAccess(), Token: "test-job-token", GitURL: "https://api.test/git/pe-1.git", ID: "attempt-test", AgentID: "agent",
		AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3", AcceleratorCount: 1,
		EstimatedDurationHours: 1, CapacityTier: clusterapi.CapacityGuaranteed, AttemptCount: attempt,
		Job: clusterapi.JobSpec{CPU: "1", Memory: "1Gi", Storage: "1Gi",
			MaxRetries: intPtr(1), TerminationGracePeriodSeconds: int64Ptr(5), AcceleratorCount: 1,
			AcceleratorType: "nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3"},
	}
}

func attemptTestClient() *JobWorkloadClient {
	return &JobWorkloadClient{apiURL: "http://registry"}
}

// The attempt travels from desired state onto the workload and back out with its phase, and the
// control plane drops any report whose attempt does not match what it is waiting on. So a break
// anywhere along that path does not fail loudly — it makes every report from this runtime look
// like it belongs to a generation nobody asked about, and the jobs go silent. Building and
// reading in one test is what pins the two ends to the same key.
func TestAWorkloadsAttemptSurvivesTheRoundTripFromDesiredStateBackToItsStatus(t *testing.T) {
	c := attemptTestClient()
	job, err := c.BuildJob(attemptTestExperiment(3), nvidiaPlacement)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}

	c.kube = chartKube(t, job)
	status, err := c.PollJobStatus(context.Background(), "attempt-test")
	if err != nil {
		t.Fatalf("PollJobStatus: %v", err)
	}
	if status.Attempt != 3 {
		t.Errorf("attempt = %d, want 3 — the generation did not survive from the built workload back to its status", status.Attempt)
	}
}

// A first attempt is a real generation. Reported as unknown it would be waved past the control
// plane's fence rather than checked by it, which is the one case the fence exists to catch.
func TestAFirstAttemptIsReportedAsZeroRatherThanUnknown(t *testing.T) {
	c := attemptTestClient()
	job, err := c.BuildJob(attemptTestExperiment(0), nvidiaPlacement)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}

	c.kube = chartKube(t, job)
	status, err := c.PollJobStatus(context.Background(), "attempt-test")
	if err != nil {
		t.Fatalf("PollJobStatus: %v", err)
	}
	if status.Attempt != 0 {
		t.Errorf("attempt = %d, want 0", status.Attempt)
	}
}

// A workload with nothing to read the generation from — one created by a build that predates the
// label — must report unknown, never a number. The control plane accepts an unknown attempt
// unfenced; a fabricated 0 would instead be checked, and would silence every job past its first
// attempt.
func TestAWorkloadWithNoAttemptLabelReportsUnknownRatherThanZero(t *testing.T) {
	c := attemptTestClient()
	job, err := c.BuildJob(attemptTestExperiment(2), nvidiaPlacement)
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	delete(job.Labels, workloadkeys.Attempt)

	c.kube = chartKube(t, job)
	status, err := c.PollJobStatus(context.Background(), "attempt-test")
	if err != nil {
		t.Fatalf("PollJobStatus: %v", err)
	}
	if status.Attempt != clusterapi.AttemptUnknown {
		t.Errorf("attempt = %d, want %d (AttemptUnknown)", status.Attempt, clusterapi.AttemptUnknown)
	}
}

// A workload that is not there at all reports Gone with no generation: there is no object to have
// carried one, and claiming a number would fence a real absence out of being seen.
func TestAnAbsentWorkloadReportsGoneWithNoAttempt(t *testing.T) {
	c := attemptTestClient()
	c.kube = chartKube(t)

	status, err := c.PollJobStatus(context.Background(), "attempt-test")
	if err != nil {
		t.Fatalf("PollJobStatus: %v", err)
	}
	if status.Phase != clusterapi.JobPhaseGone {
		t.Errorf("phase = %v, want Gone", status.Phase)
	}
	if status.Attempt != clusterapi.AttemptUnknown {
		t.Errorf("attempt = %d, want %d (AttemptUnknown)", status.Attempt, clusterapi.AttemptUnknown)
	}
}

func TestAWorkloadIsNotRecreatedWhileAPodOfItsPreviousGenerationExists(t *testing.T) {
	exp := attemptTestExperiment(2)
	c := attemptTestClient()
	c.kube = chartKube(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "old", Namespace: HypothesisLoopNamespace, Labels: map[string]string{workloadkeys.ExperimentID: exp.ID},
	}})
	if err := c.CreateWorkload(context.Background(), exp); err == nil || !strings.Contains(err.Error(), "previous generation") {
		t.Fatalf("CreateWorkload beside a pod of the previous generation returned %v, want a retry error naming it", err)
	}
	jobs, err := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("created %d job(s) beside a pod of the previous generation", len(jobs.Items))
	}
}
