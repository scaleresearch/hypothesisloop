package workloadenv

import (
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/jobenv"
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

func TestMergedKeepsThePlatformsValueOverTheJobs(t *testing.T) {
	exp := &clusterapi.DesiredJob{ID: "exp-1", Image: "registry.test/image@sha256:aa", Launch: "exec \"$@\"", ResolvedJob: gracedJob(), Data: &clusterapi.DataAccess{Region: "eu"}, Token: "job-token", GitURL: "https://api.test/git/pe-1.git"}
	spec := clusterapi.JobSpec{Env: map[string]string{"LR": "0.1", "AWS_REGION": "us"}}
	group := clusterapi.JobGroup{Env: map[string]string{"LR": "0.3", "ROLE": "learner"}}

	env, err := Merged(exp, spec, group, "http://api", func(url string) string { return url })
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"LR": "0.3", "ROLE": "learner", "AWS_REGION": "eu",
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

// The launch script reads the token, the repository and the stop grace from the environment, so
// a job stating any of them would fetch other code or outlive its grace. Admission refuses every
// name the platform sets, and the merge keeps the platform's should one get through.
func TestVarsPutThePlatformsFirstAndKeepItsValueForEveryNameItSets(t *testing.T) {
	exp := &clusterapi.DesiredJob{ID: "exp-1", Image: "registry.test/image@sha256:aa", Launch: "exec \"$@\"", ResolvedJob: gracedJob(), Data: &clusterapi.DataAccess{}, Token: "job-token", GitURL: "https://api.test/git/pe-1.git"}
	same := func(url string) string { return url }
	platform, err := Platform(exp, clusterapi.JobGroup{CPU: "2"}, "http://api", same)
	if err != nil {
		t.Fatal(err)
	}
	stated := map[string]string{"B": "job", "A": "job"}
	for _, v := range platform {
		if !jobenv.Reserved(v.Name) {
			t.Errorf("the platform sets %s, which jobenv.Reserved lets a job state", v.Name)
		}
		stated[v.Name] = "job's own"
	}
	vars, err := Vars(exp, clusterapi.JobSpec{Env: stated}, clusterapi.JobGroup{CPU: "2"}, "http://api", same)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(slices.Clone(platform), Var{"A", "job"}, Var{"B", "job"}); !slices.Equal(vars, want) {
		t.Errorf("Vars = %v, want %v", vars, want)
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

// A container sees all of its node's cores, so the job is told the whole cores its cpu holds.
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
	if _, err := Platform(exp, clusterapi.JobGroup{CPU: "four"}, "http://api", same); err == nil {
		t.Error("Platform for cpu \"four\": got nil, want an error")
	}
}

func gangEnv(vars []GangVar, index int) ([]string, map[string]string) {
	names := make([]string, 0, len(vars))
	env := map[string]string{}
	for _, v := range vars {
		names = append(names, v.Name)
		env[v.Name] = v.At(index).Value
	}
	return names, env
}

// An ungrouped node's rank is its index, and the torch names stand beside the platform's.
func TestGangTellsAnUngroupedNodeItsRank(t *testing.T) {
	spec := clusterapi.JobSpec{NumNodes: 3}
	names, env := gangEnv(Gang(spec, spec.NodeGroups()[0], 0, "master.test"), 2)
	if want := []string{
		jobenv.WorldSize, jobenv.MasterAddr, jobenv.MasterPort,
		jobenv.TorchLocalRank, jobenv.TorchWorldSize, jobenv.TorchMasterAddr, jobenv.TorchMasterPort,
		jobenv.Rank, jobenv.TorchRank,
	}; !slices.Equal(names, want) {
		t.Fatalf("names = %q, want %q", names, want)
	}
	for name, want := range map[string]string{
		"HYPOTHESISLOOP_WORLD_SIZE": "3", "HYPOTHESISLOOP_MASTER_ADDR": "master.test", "HYPOTHESISLOOP_MASTER_PORT": "29500",
		"LOCAL_RANK": "0", "WORLD_SIZE": "3", "MASTER_ADDR": "master.test", "MASTER_PORT": "29500",
		"HYPOTHESISLOOP_RANK": "2", "RANK": "2",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
}

// A grouped node is told its group, its index in it and the group's offset, and no RANK: the
// world spans every group.
func TestGangTellsAGroupedNodeItsGroupAndOffsetAndNoRank(t *testing.T) {
	spec := clusterapi.JobSpec{Groups: []clusterapi.JobGroup{{Name: "learner", Replicas: 1}, {Name: "actor", Replicas: 3}}}
	names, env := gangEnv(Gang(spec, spec.Groups[1], 1, "master.test"), 2)
	if want := []string{
		jobenv.WorldSize, jobenv.MasterAddr, jobenv.MasterPort,
		jobenv.TorchLocalRank, jobenv.TorchWorldSize, jobenv.TorchMasterAddr, jobenv.TorchMasterPort,
		jobenv.Group, jobenv.GroupRank, jobenv.GroupReplicas, jobenv.RankOffset,
	}; !slices.Equal(names, want) {
		t.Fatalf("names = %q, want %q", names, want)
	}
	for name, want := range map[string]string{
		"HYPOTHESISLOOP_WORLD_SIZE": "4", "WORLD_SIZE": "4",
		"HYPOTHESISLOOP_GROUP": "actor", "HYPOTHESISLOOP_GROUP_RANK": "2", "HYPOTHESISLOOP_GROUP_REPLICAS": "3", "HYPOTHESISLOOP_RANK_OFFSET": "1",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
}

func gracedJob() *clusterapi.JobSpec {
	shutdown, checkpoint := int64(30), int64(600)
	return &clusterapi.JobSpec{TerminationGracePeriodSeconds: &shutdown, CheckpointGraceSeconds: &checkpoint}
}
