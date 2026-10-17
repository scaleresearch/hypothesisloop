package people

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// localTemplate stands a local repository with two commits in for the experiment template.
func localTemplate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	template := filepath.Join(root, "template")
	clitest.MustGit(t, root, "init", "-q", template)
	for path, content := range map[string]string{".hypothesisloop/experiment.yaml": "name: REPLACE-WITH-NAME\n", "README.md": "REPLACE-WITH what is measured\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(template, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(template, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		clitest.MustGit(t, template, "add", path)
		clitest.MustGit(t, template, "commit", "-q", "-m", "add "+path)
	}
	old := experimentTemplateURL
	experimentTemplateURL = "file://" + template
	t.Cleanup(func() { experimentTemplateURL = old })
}

// hl repo init puts the template's files in DIR as a repository of its own: no commit of the
// template's history and no remote, so nothing pushed from it reaches the template.
func TestRepoInitLeavesTheTemplateAsARepositoryWithNoCommitAndNoRemote(t *testing.T) {
	localTemplate(t)
	dir := filepath.Join(t.TempDir(), "model")
	code, stdout, stderr := clitest.Output(t, RepoInit, dir)
	if code != 0 || !strings.Contains(stdout, "put the experiment template in "+dir) {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, path := range []string{".hypothesisloop/experiment.yaml", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Errorf("%s: %v, want the template's file", path, err)
		}
	}
	if remotes := clitest.MustGit(t, dir, "remote"); remotes != "" {
		t.Errorf("remotes = %q, want none", remotes)
	}
	if err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "-q", "HEAD").Run(); err == nil {
		t.Error("DIR has a commit, want none")
	}
	if top := clitest.MustGit(t, dir, "rev-parse", "--show-toplevel"); filepath.Base(top) != "model" {
		t.Errorf("DIR's repository is at %s, want DIR itself", top)
	}
}

func TestRepoInitRefusesADirectoryThatHoldsFilesAndWantsOneDIR(t *testing.T) {
	localTemplate(t)
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "keep.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := clitest.Output(t, RepoInit, full); code != 1 {
		t.Errorf("a directory that holds files: exit %d, stderr %q, want 1", code, stderr)
	}
	if data, err := os.ReadFile(filepath.Join(full, "keep.txt")); err != nil || string(data) != "mine\n" {
		t.Errorf("the directory's file: %q, %v, want it untouched", data, err)
	}
	for _, args := range [][]string{nil, {"a", "b"}} {
		if code, _, stderr := clitest.Output(t, RepoInit, args...); code != 2 || !strings.Contains(stderr, "want one DIR") {
			t.Errorf("%d operands: exit %d, stderr %q, want 2 asking for one DIR", len(args), code, stderr)
		}
	}
}
