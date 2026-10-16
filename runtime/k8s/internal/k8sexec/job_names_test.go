package k8sexec

import (
	"fmt"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

const (
	issuedJobID   = "job-0199c2a4b7d07f3a9c1e5b2d4f608a13"
	issuedAgentID = "ag-0199c2a4b7d07f3a9c1e5b2d4f608a13"
)

// longestID is the longest id the control plane accepts.
var longestID = strings.Repeat("a", clusterapi.MaxIdentifierLength-1) + "b"

// assertNamesAreValid holds a compiled Job to what the API server holds it to: its name is copied
// into the job-name label, each pod of an Indexed Job is reached at <name>-<index>, and every
// label value is at most 63 characters.
func assertNamesAreValid(t *testing.T, job *batchv1.Job) {
	t.Helper()
	if problems := validation.IsDNS1123Label(job.Name); len(problems) > 0 {
		t.Errorf("Job name %q (%d characters): %v", job.Name, len(job.Name), problems)
	}
	if job.Spec.Completions != nil {
		last := fmt.Sprintf("%s-%d", job.Name, *job.Spec.Completions-1)
		if problems := validation.IsDNS1123Label(last); len(problems) > 0 {
			t.Errorf("hostname %q of the last rank (%d characters): %v", last, len(last), problems)
		}
	}
	if subdomain := job.Spec.Template.Spec.Subdomain; subdomain != "" {
		if problems := validation.IsDNS1035Label(subdomain); len(problems) > 0 {
			t.Errorf("Service name %q: %v", subdomain, problems)
		}
	}
	for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
		for key, value := range labels {
			if problems := validation.IsValidLabelValue(value); len(problems) > 0 {
				t.Errorf("Job %s label %s=%q: %v", job.Name, key, value, problems)
			}
		}
	}
	if problems := validation.IsDNS1123Subdomain(resourceClaimTemplateName(job.Name)); len(problems) > 0 {
		t.Errorf("ResourceClaimTemplate name %q: %v", resourceClaimTemplateName(job.Name), problems)
	}
	if addr := envOf(job)["MASTER_ADDR"]; addr != "" {
		if problems := validation.IsDNS1123Subdomain(addr); len(problems) > 0 {
			t.Errorf("MASTER_ADDR %q: %v", addr, problems)
		}
		for _, label := range strings.Split(addr, ".") {
			if problems := validation.IsDNS1123Label(label); len(problems) > 0 {
				t.Errorf("MASTER_ADDR %q label %q: %v", addr, label, problems)
			}
		}
	}
}

func TestAJobOfAnIssuedIdKeepsItsWholeIdInEveryName(t *testing.T) {
	c := &JobWorkloadClient{apiURL: testAPIURL}
	for _, nodes := range []int{1, 2, 16, 1000} {
		exp := distributedExperiment(nodes, 0)
		exp.ID, exp.AgentID = issuedJobID, issuedAgentID
		job, err := c.BuildJob(exp, nvidiaPlacement)
		if err != nil {
			t.Fatal(err)
		}
		assertNamesAreValid(t, job)
		if job.Name != "exp-"+issuedJobID {
			t.Errorf("%d nodes: Job name = %q, want exp-%s", nodes, job.Name, issuedJobID)
		}
		if got := job.Labels[workloadkeys.ExperimentID]; got != issuedJobID {
			t.Errorf("%d nodes: experiment-id label = %q", nodes, got)
		}
		if got := job.Labels[workloadkeys.AgentID]; got != issuedAgentID {
			t.Errorf("%d nodes: agent-id label = %q", nodes, got)
		}
	}
}

func TestAMultiRankJobOfTheLongestIdsHasAHostnameForEveryRank(t *testing.T) {
	c := &JobWorkloadClient{apiURL: testAPIURL}
	for _, nodes := range []int{1, 2, 16, 1000, 100000} {
		exp := distributedExperiment(nodes, 0)
		exp.ID, exp.AgentID = longestID, longestID
		job, err := c.BuildJob(exp, nvidiaPlacement)
		if err != nil {
			t.Fatal(err)
		}
		assertNamesAreValid(t, job)
	}
}

func TestGroupsWithLongNamesThatShareTheirStartAreNamedApartWithinADNSLabel(t *testing.T) {
	c := &JobWorkloadClient{apiURL: testAPIURL}
	shared := strings.Repeat("parameter-server-", 4)
	for _, id := range []string{issuedJobID, longestID} {
		exp := groupedExperiment()
		exp.ID, exp.AgentID = id, issuedAgentID
		exp.Job.Groups[0].Name, exp.Job.Groups[0].Replicas = shared+"chief", 1
		exp.Job.Groups[1].Name, exp.Job.Groups[1].Replicas = shared+"worker", 1000
		jobs, err := c.BuildJobs(exp, AcceleratorPlacement{DeviceClassName: "tenstorrent.com"})
		if err != nil {
			t.Fatal(err)
		}
		if jobs[0].Name == jobs[1].Name {
			t.Fatalf("id %q: both groups compile to a Job named %q", id, jobs[0].Name)
		}
		if jobs[0].Labels[workloadkeys.JobGroup] == jobs[1].Labels[workloadkeys.JobGroup] {
			t.Fatalf("id %q: both groups carry the group label %q", id, jobs[0].Labels[workloadkeys.JobGroup])
		}
		for _, job := range jobs {
			assertNamesAreValid(t, job)
			if id == issuedJobID && !strings.HasPrefix(job.Name, "exp-"+issuedJobID+"-") {
				t.Errorf("Job name %q no longer starts with its whole experiment id", job.Name)
			}
		}
		want := jobs[0].Name + "-0." + jobs[0].Spec.Template.Spec.Subdomain + "." + HypothesisLoopNamespace + ".svc.cluster.local"
		if got := envOf(jobs[1])["MASTER_ADDR"]; got != want {
			t.Errorf("id %q: MASTER_ADDR = %q, want %q", id, got, want)
		}
	}
}
