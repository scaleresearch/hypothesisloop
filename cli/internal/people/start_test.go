package people

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

const startSpec = `name: tune the model
description: beat the baseline in the README
budget_accelerator_hours: 40
max_agents: 6
duration_hours: 400
metrics:
  - key: val_accuracy
    direction: maximize
project_id: prj_1
repository: acme/model
branch: exp
`

// startDir is an experiment's tree in its own git repository, on a branch named other than the
// one its file states, and a bare origin that has none of it.
func startDir(t *testing.T) (dir, origin string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(key, "t")
	}
	for _, key := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(key, "t@t")
	}
	root := t.TempDir()
	origin, dir = filepath.Join(root, "origin.git"), filepath.Join(root, "model")
	clitest.MustGit(t, root, "init", "-q", "--bare", origin)
	for path, content := range map[string]string{".hypothesisloop/experiment.yaml": startSpec, "train.py": "print(1)\n", "README.md": "what is measured\n", ".gitignore": "out/\n", "out/ckpt": "ignored\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	clitest.MustGit(t, dir, "init", "-q", "-b", "local-work")
	return dir, origin
}

func startServer(t *testing.T, origin string) *[]clitest.Recorded {
	return clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
		if strings.HasSuffix(r.Path, "/repositories") {
			return map[string]string{"repository": "Acme/model", "repository_url": origin}
		}
		return map[string]any{"id": "pe-1", "status": "open", "status_reason": map[string]string{"summary": "its baseline is measured"}}
	})
}

// A branch the repository does not have is pushed as DIR in one commit, .gitignore respected, and
// the experiment is created on it under the name the platform gave the repository.
func TestStartPushesDIRAsABranchTheRepositoryDoesNotHave(t *testing.T) {
	dir, origin := startDir(t)
	calls := startServer(t, origin)
	code, stdout, stderr := clitest.Output(t, PlatformExperimentsStart, dir)
	if code != 0 || !strings.Contains(stdout, "pushed 4 files of "+dir+" to Acme/model as exp") || !strings.Contains(stdout, "created pe-1, open: its baseline is measured") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if files := clitest.MustGit(t, origin, "ls-tree", "-r", "--name-only", "exp"); files != ".gitignore\n.hypothesisloop/experiment.yaml\nREADME.md\ntrain.py" {
		t.Errorf("branch exp holds %q, want DIR without what .gitignore leaves out", files)
	}
	if commits := clitest.MustGit(t, origin, "rev-list", "--count", "exp"); commits != "1" {
		t.Errorf("branch exp has %s commits, want one", commits)
	}
	if len(*calls) != 3 || (*calls)[0].Path != "/projects/prj_1/repositories" || (*calls)[0].Body["repository"] != "acme/model" {
		t.Fatalf("calls = %+v, want the repository asked for in the file's project", *calls)
	}
	if checked := (*calls)[1]; checked.Path != "/platform-experiments/check" || checked.Body["repository"] != "Acme/model" || checked.Body["name"] != "tune the model" {
		t.Errorf("check = %+v, want the file's experiment checked in the repository as the platform names it", checked)
	}
	if created := (*calls)[2]; created.Path != "/platform-experiments" || created.Body["repository"] != "Acme/model" || created.Body["branch"] != "exp" || created.Body["project_id"] != "prj_1" {
		t.Errorf("create = %+v, want the file's placement with the repository as the platform names it", created)
	}
	if head := clitest.MustGit(t, dir, "symbolic-ref", "--short", "HEAD"); head != "local-work" {
		t.Errorf("DIR's own repository is on %q, want it untouched", head)
	}
}

// A branch that holds DIR already gets nothing pushed, and the experiment is created on it.
func TestStartPushesNothingToABranchThatHoldsDIR(t *testing.T) {
	dir, origin := startDir(t)
	startServer(t, origin)
	if code, _, stderr := clitest.Output(t, PlatformExperimentsStart, dir); code != 0 {
		t.Fatalf("first start: exit %d: %s", code, stderr)
	}
	before := clitest.MustGit(t, origin, "rev-parse", "exp")
	calls := startServer(t, origin)
	code, stdout, stderr := clitest.Output(t, PlatformExperimentsStart, dir)
	if code != 0 || !strings.Contains(stdout, "branch exp of Acme/model already holds "+dir+": pushed nothing") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if after := clitest.MustGit(t, origin, "rev-parse", "exp"); after != before {
		t.Errorf("branch exp moved from %s to %s", before, after)
	}
	if len(*calls) != 3 || (*calls)[2].Path != "/platform-experiments" {
		t.Errorf("calls = %+v, want the experiment created", *calls)
	}
}

// A branch that holds other code exits 1, pushes nothing and creates nothing: nothing is ever
// force-pushed.
func TestStartRefusesABranchThatHoldsOtherCode(t *testing.T) {
	dir, origin := startDir(t)
	startServer(t, origin)
	if code, _, stderr := clitest.Output(t, PlatformExperimentsStart, dir); code != 0 {
		t.Fatalf("first start: exit %d: %s", code, stderr)
	}
	before := clitest.MustGit(t, origin, "rev-parse", "exp")
	if err := os.WriteFile(filepath.Join(dir, "train.py"), []byte("print(2)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := startServer(t, origin)
	code, _, stderr := clitest.Output(t, PlatformExperimentsStart, dir)
	if code != 1 || !strings.Contains(stderr, "branch exp of Acme/model holds other code than "+dir) || !strings.Contains(stderr, "name a branch that does not exist") {
		t.Fatalf("exit %d, stderr %q, want 1 naming the branch and what to do", code, stderr)
	}
	if after := clitest.MustGit(t, origin, "rev-parse", "exp"); after != before {
		t.Errorf("branch exp moved from %s to %s", before, after)
	}
	if len(*calls) != 2 {
		t.Errorf("calls = %+v, want the repository read, the experiment checked and none created", *calls)
	}
}

// An experiment the platform refuses exits 1 with its refusal before anything is pushed, so
// starting again once the file is fixed pushes the branch then.
func TestStartPushesNothingForAnExperimentThePlatformRefuses(t *testing.T) {
	dir, origin := startDir(t)
	var paths []string
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/repositories") {
			_ = json.NewEncoder(w).Encode(map[string]string{"repository": "Acme/model", "repository_url": origin})
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{"reason": "model_not_served", "message": "agents[0].model is not served: anthropic serves no model of that id"})
	}))
	code, _, stderr := clitest.Output(t, PlatformExperimentsStart, dir)
	if code != 1 || !strings.Contains(stderr, "agents[0].model is not served") {
		t.Fatalf("exit %d, stderr %q, want 1 with the refusal", code, stderr)
	}
	if heads := clitest.MustGit(t, origin, "for-each-ref", "refs/heads"); heads != "" {
		t.Errorf("origin holds %q, want no branch pushed", heads)
	}
	if strings.Join(paths, " ") != "/projects/prj_1/repositories /platform-experiments/check" {
		t.Errorf("calls = %v, want the repository and the check and no create", paths)
	}
}

// A file that is no valid experiment exits 2 before anything is asked.
func TestStartChecksTheFileBeforeAskingAnything(t *testing.T) {
	dir, origin := startDir(t)
	for name, file := range map[string]string{
		"no project":           strings.Replace(startSpec, "project_id: prj_1\n", "", 1),
		"repository no branch": strings.Replace(startSpec, "branch: exp\n", "", 1),
		"no metrics":           strings.Replace(startSpec, "metrics:\n  - key: val_accuracy\n    direction: maximize\n", "", 1),
	} {
		if err := os.WriteFile(filepath.Join(dir, ".hypothesisloop/experiment.yaml"), []byte(file), 0o644); err != nil {
			t.Fatal(err)
		}
		calls := startServer(t, origin)
		if code, _, stderr := clitest.Output(t, PlatformExperimentsStart, dir); code != 2 || len(*calls) != 0 {
			t.Errorf("%s: exit %d, %d calls, stderr %q, want 2 and nothing asked", name, code, len(*calls), stderr)
		}
	}
}
