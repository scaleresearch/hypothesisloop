package k8sexec

import "testing"

// A cluster given a job image tells the control plane, which then sends it the jobs that name
// no image, and their pods run in it. A job that names its own keeps its own.
func TestAJobThatNamesNoImageRunsInTheClustersJobImage(t *testing.T) {
	c := &JobWorkloadClient{apiURL: testAPIURL, jobImage: "registry.example/job:1"}
	if c.JobImage() != "registry.example/job:1" {
		t.Fatalf("job image reported = %q", c.JobImage())
	}
	named := attemptTestExperiment(0)
	unnamed := attemptTestExperiment(0)
	unnamed.Job.Image = ""
	for _, tc := range []struct {
		image, want string
		job         func() (string, error)
	}{
		{named.Job.Image, named.Job.Image, func() (string, error) {
			job, err := c.BuildJob(named, nvidiaPlacement)
			if err != nil {
				return "", err
			}
			return job.Spec.Template.Spec.Containers[0].Image, nil
		}},
		{"", "registry.example/job:1", func() (string, error) {
			job, err := c.BuildJob(unnamed, nvidiaPlacement)
			if err != nil {
				return "", err
			}
			return job.Spec.Template.Spec.Containers[0].Image, nil
		}},
	} {
		got, err := tc.job()
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("a job naming image %q runs in %q, want %q", tc.image, got, tc.want)
		}
	}
}
