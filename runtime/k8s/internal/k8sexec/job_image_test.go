package k8sexec

import "testing"

// A pod runs in the image its job's desired state names. A job that reaches the cluster with
// none is not started: the control plane decides the image, and the cluster has none to put in.
func TestAPodRunsInExactlyTheImageDesiredStateNames(t *testing.T) {
	c := &JobWorkloadClient{apiURL: testAPIURL}
	if c.OnlyImage() != "" {
		t.Fatalf("the cluster reports it runs only %q, want any image", c.OnlyImage())
	}
	named := attemptTestExperiment(0)
	job, err := c.BuildJob(named, nvidiaPlacement)
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != named.Job.Image {
		t.Errorf("the pod runs in %q, want %q", got, named.Job.Image)
	}
	unnamed := attemptTestExperiment(0)
	unnamed.Job.Image = ""
	if _, err := c.BuildJob(unnamed, nvidiaPlacement); err == nil {
		t.Error("a job with no image in desired state was built")
	}
}
