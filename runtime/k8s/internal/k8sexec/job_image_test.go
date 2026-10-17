package k8sexec

import "testing"

// A pod runs in the image its job's desired state names. A job that reaches the cluster with
// none is not started: the control plane decides the image, and the cluster has none to put in.
func TestAPodRunsInExactlyTheImageDesiredStateNames(t *testing.T) {
	c := &JobWorkloadClient{apiURL: testAPIURL}
	named := attemptTestExperiment(0)
	job, err := c.BuildJob(named, nvidiaPlacement)
	if err != nil {
		t.Fatal(err)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != named.Image {
		t.Errorf("the pod runs in %q, want %q", got, named.Image)
	}
	unnamed := attemptTestExperiment(0)
	unnamed.Image = ""
	if _, err := c.BuildJob(unnamed, nvidiaPlacement); err == nil {
		t.Error("a job with no image in desired state was built")
	}
}

const testImage = "registry.test/image@sha256:abababababababababababababababababababababababababababababababab"
