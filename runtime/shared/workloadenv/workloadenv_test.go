package workloadenv

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestPlatformRewritesOnlyTheURLsAndRefusesAJobWithNowhereToSave(t *testing.T) {
	exp := &domain.Experiment{ID: "exp-1", AgentID: "agent-1", AttemptCount: 2,
		Data: &domain.DataAccess{Endpoint: "http://localhost:9000", URI: "s3://bucket/exp-1"}, Token: "job-token"}
	reach := func(url string) string { return url + "/reached" }

	vars, err := Platform(exp, domain.JobGroup{AcceleratorCount: 4}, "http://localhost:8081", reach)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range vars {
		got[v.Name] = v.Value
	}
	for name, want := range map[string]string{
		"AWS_ENDPOINT_URL":                 "http://localhost:9000/reached",
		"AWS_ENDPOINT_URL_S3":              "http://localhost:9000/reached",
		"HYPOTHESISLOOP_API_URL":           "http://localhost:8081/reached",
		"HYPOTHESISLOOP_DATA_URI":          "s3://bucket/exp-1",
		"HYPOTHESISLOOP_ACCELERATOR_COUNT": "4",
		"HYPOTHESISLOOP_ATTEMPT":           "2",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}

	exp.Data = nil
	if _, err := Platform(exp, domain.JobGroup{}, "http://localhost:8081", reach); err == nil {
		t.Error("Platform for an experiment without durable-data access: got nil, want an error")
	}
}

// The k8s runtime hashes the variable list, so a variable added anywhere but the end would
// change the hash of every position after it.
func TestPlatformEmitsTheJobTokenLastAndRefusesAJobWithoutOne(t *testing.T) {
	exp := &domain.Experiment{ID: "exp-1", Data: &domain.DataAccess{Endpoint: "http://store:9000"}, Token: "job-token"}
	same := func(url string) string { return url }
	vars, err := Platform(exp, domain.JobGroup{}, "http://api:8081", same)
	if err != nil {
		t.Fatal(err)
	}
	if last := vars[len(vars)-1]; last.Name != "HYPOTHESISLOOP_TOKEN" || last.Value != "job-token" {
		t.Errorf("last variable = %+v, want HYPOTHESISLOOP_TOKEN=job-token", last)
	}
	exp.Token = ""
	if _, err := Platform(exp, domain.JobGroup{}, "http://api:8081", same); err == nil {
		t.Error("Platform for an experiment without a job token: got nil, want an error")
	}
}

func TestJobLayersTheGroupEnvironmentOverTheJobs(t *testing.T) {
	got := Job(domain.JobSpec{Env: map[string]string{"A": "job", "B": "job"}}, domain.JobGroup{Env: map[string]string{"B": "group"}})
	if got["A"] != "job" || got["B"] != "group" {
		t.Errorf("Job = %v, want A from the job and B from the group", got)
	}
}
