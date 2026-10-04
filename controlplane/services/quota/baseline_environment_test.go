package quota

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

const branchEnvironment = "ARG BASE\nFROM ${BASE}\nRUN apt-get install -y rustc\n"

// ranIn is the baseline job run in the environment of file, as admission places it.
func ranIn(b *Baseline, file string) {
	env := &domain.Environment{Key: domain.EnvironmentKey("org", file, "registry.test/job:1", ""), OrgID: "org", Dockerfile: file, Image: "registry.test/env@sha256:job", State: domain.EnvironmentReady}
	b.Job.EnvironmentKey, b.Job.ResolvedJob, b.Environment = env.Key, &domain.JobSpec{Image: env.Image}, env
}

// When the baseline branch declares an environment, the baseline is the job that ran in it, so a
// job of the default branch runs with no setup in the environment the baseline was measured in.
func TestTheBaselineIsHeldToTheEnvironmentItsBranchDeclares(t *testing.T) {
	declaration := map[string]any{"baseline_job_id": "job-1"}
	declares := func(file string) *string { return &file }

	baseline := measuredBaseline()
	ranIn(baseline, branchEnvironment)
	baseline.Declared = declares(branchEnvironment)
	store, patch := declareAPI(t, openExperiment(), baseline, theCoordinator)
	if code, out := patch(declaration); code != http.StatusOK || store.created[0].BaselineJobID != "job-1" {
		t.Fatalf("got = %d %v, want the baseline declared", code, out)
	}

	for name, c := range map[string]struct {
		change func(*Baseline)
		reason string
	}{
		"a job from before the branch declared one": {func(b *Baseline) { b.Job.EnvironmentKey, b.Environment = "", nil }, "baseline_environment_differs"},
		"a job in another file":                     {func(b *Baseline) { ranIn(b, branchEnvironment+"RUN true\n") }, "baseline_environment_differs"},
		"a branch that dropped the file":            {func(b *Baseline) { b.Declared = nil }, "baseline_environment_differs"},
		"a job not run in its image":                {func(b *Baseline) { b.Job.ResolvedJob.Image = "registry.test/job:1" }, "baseline_not_in_environment"},
	} {
		baseline := measuredBaseline()
		ranIn(baseline, branchEnvironment)
		baseline.Declared = declares(branchEnvironment)
		c.change(baseline)
		store, patch := declareAPI(t, openExperiment(), baseline, theCoordinator)
		if code, out := patch(declaration); code != http.StatusUnprocessableEntity || out["reason"] != c.reason || store.created[0].BaselineJobID != "" {
			t.Errorf("%s: got = %d %v, want 422 %s and nothing stored", name, code, out, c.reason)
		}
	}

	// A branch with no file and a job with none declare as before.
	store, patch = declareAPI(t, openExperiment(), measuredBaseline(), theCoordinator)
	if code, out := patch(declaration); code != http.StatusOK {
		t.Errorf("no environment: got = %d %v, want declared", code, out)
	}
}

// A job placed on the builder's node runs the image loaded there before the push ends: that is
// the environment's image, and the baseline it measured is declared.
func TestABaselineRunInTheImageLoadedBeforeItsPushIsDeclared(t *testing.T) {
	baseline := measuredBaseline()
	ranIn(baseline, branchEnvironment)
	baseline.Declared = &[]string{branchEnvironment}[0]
	baseline.Environment.LoadedImage, baseline.Environment.Image, baseline.Environment.State = baseline.Environment.Image, "", domain.EnvironmentBuilding
	store, patch := declareAPI(t, openExperiment(), baseline, theCoordinator)
	if code, out := patch(map[string]any{"baseline_job_id": "job-1"}); code != http.StatusOK || store.created[0].BaselineJobID != "job-1" {
		t.Fatalf("got = %d %v, want the baseline declared", code, out)
	}
}

// Every reason a baseline is refused for becomes the experiment's status_reason, and reading an
// experiment whose reason names nobody it waits for fails.
func TestEveryBaselineRefusalNamesWhoActsOnIt(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	refusal := regexp.MustCompile(`refusedBaseline\("([a-z_]+)"`)
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range refusal.FindAllStringSubmatch(string(source), -1) {
			if _, err := domain.WaitsFor(m[1], true); err != nil {
				t.Errorf("%s: %v", file, err)
			}
		}
	}
}
