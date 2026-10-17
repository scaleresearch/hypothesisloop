package domain

import "testing"

func TestAClusterRunsTheJobsItsImageRuleAdmits(t *testing.T) {
	const fixed = "registry.test/platform-job:7"
	rented, k8s := Cluster{OrgID: "org-a", JobImage: fixed}, Cluster{OrgID: "org-a"}
	for _, c := range []struct {
		name    string
		cluster Cluster
		image   string
		want    bool
	}{
		{"a fixed image has nothing to run a job with none in either", rented, "", false},
		{"a fixed image runs a job that names it", rented, fixed, true},
		{"a fixed image does not run another", rented, "registry.test/mine:1", false},
		{"a fixed image does not run another tag of itself", rented, "registry.test/platform-job:8", false},
		{"no fixed image runs the image named", k8s, "registry.test/mine:1", true},
		{"no fixed image has nothing to run a job with none in", k8s, "", false},
	} {
		if got := c.cluster.Runs(c.image); got != c.want {
			t.Errorf("%s: Runs(%q) = %v, want %v", c.name, c.image, got, c.want)
		}
		if got := c.cluster.HostsJob("org-a", c.image); got != c.want {
			t.Errorf("%s: HostsJob of its own org = %v, want %v", c.name, got, c.want)
		}
		if c.cluster.HostsJob("org-b", c.image) {
			t.Errorf("%s: a job of another org is hosted", c.name)
		}
	}
}
