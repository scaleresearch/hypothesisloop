package workloadenv

import (
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

func TestPlatformRewritesOnlyTheURLsAndRefusesAJobWithNowhereToSave(t *testing.T) {
	exp := &clusterapi.DesiredJob{ID: "exp-1", Image: "registry.test/image@sha256:aa", Launch: "exec \"$@\"", ResolvedJob: gracedJob(), AgentID: "agent-1", AttemptCount: 2,
		Data: &clusterapi.DataAccess{Endpoint: "http://localhost:9000", URI: "s3://bucket/exp-1"}, Token: "job-token", GitURL: "https://api.test/git/pe-1.git"}
	reach := func(url string) string { return url + "/reached" }

	vars, err := Platform(exp, clusterapi.JobGroup{AcceleratorCount: 4}, "http://localhost:8081", reach)
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
		"HYPOTHESISLOOP_GIT_URL":           "https://api.test/git/pe-1.git/reached",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}

	exp.Data = nil
	if _, err := Platform(exp, clusterapi.JobGroup{}, "http://localhost:8081", reach); err == nil {
		t.Error("Platform for an experiment without durable-data access: got nil, want an error")
	}
}

// The k8s runtime hashes the variable list, so a variable added anywhere but the end would
// change the hash of every position after it. The repository address came after the token.
func TestPlatformEmitsTheJobTokenAndRepositoryLastAndRefusesAJobWithoutThem(t *testing.T) {
	exp := &clusterapi.DesiredJob{ID: "exp-1", Image: "registry.test/image@sha256:aa", Launch: "exec \"$@\"", ResolvedJob: gracedJob(), Data: &clusterapi.DataAccess{Endpoint: "http://store:9000"}, Token: "job-token", GitURL: "https://api.test/git/pe-1.git"}
	same := func(url string) string { return url }
	vars, err := Platform(exp, clusterapi.JobGroup{}, "http://api:8081", same)
	if err != nil {
		t.Fatal(err)
	}
	if token, repository := vars[len(vars)-3], vars[len(vars)-2]; token != (Var{"HYPOTHESISLOOP_TOKEN", "job-token"}) || repository != (Var{"HYPOTHESISLOOP_GIT_URL", exp.GitURL}) {
		t.Errorf("last variables = %+v, %+v, want HYPOTHESISLOOP_TOKEN=job-token, then HYPOTHESISLOOP_GIT_URL, before the stop grace", token, repository)
	}
	exp.GitURL = ""
	if _, err := Platform(exp, clusterapi.JobGroup{}, "http://api:8081", same); err == nil {
		t.Error("Platform for an experiment without a repository address: got nil, want an error")
	}
	exp.Token = ""
	if _, err := Platform(exp, clusterapi.JobGroup{}, "http://api:8081", same); err == nil {
		t.Error("Platform for an experiment without a job token: got nil, want an error")
	}
}

func TestJobLayersTheGroupEnvironmentOverTheJobs(t *testing.T) {
	got := Job(clusterapi.JobSpec{Env: map[string]string{"A": "job", "B": "job"}}, clusterapi.JobGroup{Env: map[string]string{"B": "group"}})
	if got["A"] != "job" || got["B"] != "group" {
		t.Errorf("Job = %v, want A from the job and B from the group", got)
	}
}

func TestMergedKeepsTheJobsOwnValueOverThePlatforms(t *testing.T) {
	exp := &clusterapi.DesiredJob{ID: "exp-1", Image: "registry.test/image@sha256:aa", Launch: "exec \"$@\"", ResolvedJob: gracedJob(), Data: &clusterapi.DataAccess{Region: "eu"}, Token: "job-token", GitURL: "https://api.test/git/pe-1.git"}
	spec := clusterapi.JobSpec{Env: map[string]string{"LR": "0.1", "AWS_REGION": "us"}}
	group := clusterapi.JobGroup{Env: map[string]string{"LR": "0.3", "ROLE": "learner"}}

	env, err := Merged(exp, spec, group, "http://api", func(url string) string { return url })
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"LR": "0.3", "ROLE": "learner", "AWS_REGION": "us",
		"HYPOTHESISLOOP_EXPERIMENT_ID": "exp-1", "HYPOTHESISLOOP_API_URL": "http://api", "HYPOTHESISLOOP_TOKEN": "job-token",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	exp.Data = nil
	if _, err := Merged(exp, spec, group, "http://api", func(url string) string { return url }); err == nil {
		t.Error("Merged for an experiment without durable-data access: got nil, want an error")
	}
}

func TestCommandIsTheLaunchScriptAroundTheGroupsOwnWhenItNamesOne(t *testing.T) {
	exp := &clusterapi.DesiredJob{Launch: "exec \"$@\""}
	spec := clusterapi.JobSpec{Command: []string{"python", "train.py"}, Args: []string{"--all"}}
	if got, want := Command(exp, spec, clusterapi.JobGroup{}), clusterapi.LaunchCommand(exp.Launch, spec.Command, spec.Args); !slices.Equal(got, want) {
		t.Errorf("no group command: %q, want %q", got, want)
	}
	// The group's command comes with the group's args, never the job's.
	group := clusterapi.JobGroup{Command: []string{"python", "actor.py"}}
	if got, want := Command(exp, spec, group), clusterapi.LaunchCommand(exp.Launch, group.Command, nil); !slices.Equal(got, want) {
		t.Errorf("group command: %q, want %q", got, want)
	}
}

func TestJobCarriesTheSecretEnvironment(t *testing.T) {
	spec := clusterapi.JobSpec{Env: map[string]string{"LR": "0.1"}, SecretEnv: map[string]string{"HF_TOKEN": "secret"}}
	got := Job(spec, clusterapi.JobGroup{Env: map[string]string{"ROLE": "learner"}})
	if got["HF_TOKEN"] != "secret" || got["LR"] != "0.1" || got["ROLE"] != "learner" {
		t.Errorf("Job = %v, want the secret beside the job's and the group's own", got)
	}
}

// A container sees all of its node's cores, so the job is told the whole cores its cpu holds,
// and a thread pool it sizes itself still wins.
func TestPlatformTellsTheJobTheCoresItHolds(t *testing.T) {
	exp := &clusterapi.DesiredJob{ID: "exp-1", Image: "registry.test/image@sha256:aa", Launch: "exec \"$@\"", ResolvedJob: gracedJob(), Data: &clusterapi.DataAccess{}, Token: "job-token", GitURL: "https://api.test/git/pe-1.git"}
	same := func(url string) string { return url }
	for cpu, want := range map[string]string{"4": "4", "500m": "1", "2.5": "3", "": ""} {
		env, err := Merged(exp, clusterapi.JobSpec{}, clusterapi.JobGroup{CPU: cpu}, "http://api", same)
		if err != nil {
			t.Fatal(err)
		}
		if env["HYPOTHESISLOOP_CPUS"] != want || env["OMP_NUM_THREADS"] != want {
			t.Errorf("cpu %q: HYPOTHESISLOOP_CPUS %q, OMP_NUM_THREADS %q; want %q", cpu, env["HYPOTHESISLOOP_CPUS"], env["OMP_NUM_THREADS"], want)
		}
	}
	env, err := Merged(exp, clusterapi.JobSpec{Env: map[string]string{"OMP_NUM_THREADS": "1"}}, clusterapi.JobGroup{CPU: "8"}, "http://api", same)
	if err != nil || env["OMP_NUM_THREADS"] != "1" || env["HYPOTHESISLOOP_CPUS"] != "8" {
		t.Errorf("env %v, %v; want the job's own OMP_NUM_THREADS kept beside 8 cores", env, err)
	}
	if _, err := Platform(exp, clusterapi.JobGroup{CPU: "four"}, "http://api", same); err == nil {
		t.Error("Platform for cpu \"four\": got nil, want an error")
	}
}

func gracedJob() *clusterapi.JobSpec {
	shutdown, checkpoint := int64(30), int64(600)
	return &clusterapi.JobSpec{TerminationGracePeriodSeconds: &shutdown, CheckpointGraceSeconds: &checkpoint}
}
