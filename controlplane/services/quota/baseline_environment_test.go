package quota

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// agentEnvironments builds every file on the agent image into an environment in state.
type agentEnvironments struct {
	state domain.EnvironmentState
	bases []string
}

func (e *agentEnvironments) Read(context.Context, string, string, string) (*string, error) {
	panic("the gate reads the branch through the baseline reader")
}

func (e *agentEnvironments) Get(context.Context, string) (*domain.Environment, error) {
	panic("the gate reads the job's environment through the baseline reader")
}

func (e *agentEnvironments) Ensure(_ context.Context, dockerfile, base string) (*domain.Environment, error) {
	if err := domain.ValidateEnvironmentFile(dockerfile); err != nil {
		return nil, err
	}
	e.bases = append(e.bases, base)
	return &domain.Environment{Key: domain.EnvironmentKey(dockerfile, base), Base: base, Dockerfile: dockerfile, Image: "registry.test/env:" + base,
		State: e.state, LogTail: "E: Unable to locate package rustc", CreatedAt: time.Now()}, nil
}

const branchEnvironment = "ARG BASE\nFROM ${BASE}\nRUN apt-get install -y rustc\n"

// ranIn is the baseline job run in the environment of file, as admission places it.
func ranIn(b *Baseline, file string) {
	env := &domain.Environment{Key: domain.EnvironmentKey(file, "registry.test/job:1"), Dockerfile: file, Image: "registry.test/env:job", State: domain.EnvironmentReady}
	b.Job.EnvironmentKey, b.Job.ResolvedJob, b.Environment = env.Key, &domain.JobSpec{Image: env.Image}, env
}

// When the baseline branch declares an environment, the baseline is the job that ran in it, and
// the agents start in the same file built on their own image once it is: the declaration waits
// for that build and is refused with its output when it fails.
func TestTheBaselineIsHeldToTheEnvironmentItsBranchDeclaresAndTheAgentsStartInIt(t *testing.T) {
	declaration := map[string]any{"baseline_job_id": "job-1"}
	declares := func(file string) *string { return &file }

	pe, baseline := openExperiment(), measuredBaseline()
	pe.AgentImage = "registry.test/agent:1"
	ranIn(baseline, branchEnvironment)
	baseline.Declared = declares(branchEnvironment)
	environments := &agentEnvironments{state: domain.EnvironmentReady}
	store, patch := declareAPI(t, pe, baseline, theCoordinator, func(s *PlatformExperimentsService) {
		s.WithEnvironments(environments, "registry.test/platform-agent:1")
	})
	code, out := patch(declaration)
	if want := domain.EnvironmentKey(branchEnvironment, "registry.test/agent:1"); code != http.StatusOK || store.created[0].AgentEnvironmentKey != want || len(environments.bases) != 1 || environments.bases[0] != "registry.test/agent:1" {
		t.Fatalf("got = %d %v, agent environment %q built on %v, want %s on the experiment's agent image", code, out, store.created[0].AgentEnvironmentKey, environments.bases, want)
	}

	for name, c := range map[string]struct {
		change func(*Baseline)
		state  domain.EnvironmentState
		status int
		reason string
	}{
		"a job from before the branch declared one": {func(b *Baseline) { b.Job.EnvironmentKey, b.Environment = "", nil }, domain.EnvironmentReady, 422, "baseline_environment_differs"},
		"a job in another file":                     {func(b *Baseline) { ranIn(b, branchEnvironment+"RUN true\n") }, domain.EnvironmentReady, 422, "baseline_environment_differs"},
		"a branch that dropped the file":            {func(b *Baseline) { b.Declared = nil }, domain.EnvironmentReady, 422, "baseline_environment_differs"},
		"a job not run in its image":                {func(b *Baseline) { b.Job.ResolvedJob.Image = "registry.test/job:1" }, domain.EnvironmentReady, 422, "baseline_not_in_environment"},
		"the agents' environment building":          {nil, domain.EnvironmentBuilding, 409, domain.NotAdmittedEnvironmentBuilding},
		"the agents' environment failed":            {nil, domain.EnvironmentFailed, 422, string(domain.EvictionEnvironmentFailed)},
	} {
		pe, baseline := openExperiment(), measuredBaseline()
		ranIn(baseline, branchEnvironment)
		baseline.Declared = declares(branchEnvironment)
		if c.change != nil {
			c.change(baseline)
		}
		environments := &agentEnvironments{state: c.state}
		store, patch := declareAPI(t, pe, baseline, theCoordinator, func(s *PlatformExperimentsService) {
			s.WithEnvironments(environments, "registry.test/platform-agent:1")
		})
		code, out := patch(declaration)
		message, _ := out["message"].(string)
		if code != c.status || out["reason"] != c.reason || store.created[0].BaselineJobID != "" || store.created[0].AgentEnvironmentKey != "" {
			t.Errorf("%s: got = %d %v, want %d %s and nothing stored", name, code, out, c.status, c.reason)
		}
		if c.state == domain.EnvironmentFailed && !strings.HasSuffix(message, "E: Unable to locate package rustc") {
			t.Errorf("%s: message %q, want the end of the build's output", name, message)
		}
	}

	// A branch with no file and a job with none start the agents in the agent image as before.
	store, patch = declareAPI(t, openExperiment(), measuredBaseline(), theCoordinator)
	if code, out := patch(declaration); code != http.StatusOK || store.created[0].AgentEnvironmentKey != "" {
		t.Errorf("no environment: got = %d %v, agent environment %q, want declared with none", code, out, store.created[0].AgentEnvironmentKey)
	}
}
