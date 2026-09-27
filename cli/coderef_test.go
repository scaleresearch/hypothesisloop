package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testBranch is the branch pushedClone leaves HEAD on: jane's implementation of h1 in pe-1.
const testBranch = "hl/pe-1/jane/idea-abc123"

// pushedClone returns a clone of a local bare origin whose default branch is pushed, with HEAD
// on testBranch, which origin does not have yet.
func pushedClone(t *testing.T) (clone, origin string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	clone = filepath.Join(root, "clone")
	mustGit(t, root, "init", "-q", "--bare", origin)
	mustGit(t, root, "clone", "-q", origin, clone)
	writeJob(t, filepath.Join(clone, "job.yaml"), "metadata:\n  hypothesis_id: h1\njob:\n  image: foo\n")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-q", "-m", "init")
	mustGit(t, clone, "push", "-q", "origin", "HEAD")
	mustGit(t, clone, "checkout", "-q", "-b", testBranch)
	t.Chdir(clone)
	return clone, origin
}

func writeJob(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// submit runs `hl job submit` on path in pe-1 and returns the exit code, stderr and the metadata
// the API received (nil when nothing was sent).
func submit(t *testing.T, path string) (int, string, map[string]any) {
	t.Helper()
	var sent map[string]any
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body["metadata"].(map[string]any)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-1"})
	}))
	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "submit", "--agent", "jane", "--platform-experiment", "pe-1", path}, &stdout, &stderr)
	return code, stderr.String(), sent
}

// A submit pushes the branch HEAD is on and sends the commit and the branch, so an agent runs no
// git push of its own and a job never names a commit origin lacks.
func TestJobSubmitPushesItsBranchAndSendsTheCommitAndTheBranch(t *testing.T) {
	clone, origin := pushedClone(t)
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "local only")
	code, stderr, sent := submit(t, "job.yaml")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	head := mustGit(t, clone, "rev-parse", "HEAD")
	if sent["code_ref"] != origin+"@"+head || sent["branch"] != testBranch {
		t.Fatalf("sent code_ref %v on branch %v, want %s@%s on %s", sent["code_ref"], sent["branch"], origin, head, testBranch)
	}
	if pushed := mustGit(t, origin, "rev-parse", "refs/heads/"+testBranch); pushed != head {
		t.Fatalf("origin has %s at %s, want the submitted commit %s", testBranch, pushed, head)
	}
}

// A job runs one agent's implementation of a hypothesis, which is a branch of the platform
// experiment. From the default branch, a detached HEAD or another experiment's branch nothing
// is pushed and nothing is sent, and the refusal names the command that makes the branch.
func TestJobSubmitRefusesAHeadOnNoBranchOfThePlatformExperiment(t *testing.T) {
	clone, origin := pushedClone(t)
	before := mustGit(t, origin, "for-each-ref")
	defaultBranch := mustGit(t, origin, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	for name, checkout := range map[string][]string{
		"the default branch":          {"checkout", "-q", defaultBranch},
		"a detached HEAD":             {"checkout", "-q", "--detach"},
		"another experiment's branch": {"checkout", "-q", "-b", "hl/pe-2/jane/idea-abc123"},
		"a branch of no experiment":   {"checkout", "-q", "-b", "scratch"},
	} {
		mustGit(t, clone, "checkout", "-q", testBranch)
		mustGit(t, clone, checkout...)
		code, stderr, sent := submit(t, "job.yaml")
		if code != 1 || sent != nil || !strings.Contains(stderr, "not on a branch under hl/pe-1/") || !strings.Contains(stderr, "hl hypothesis checkout h1") {
			t.Errorf("%s: exit code = %d, sent = %v, stderr = %s, want it refused naming hl hypothesis checkout h1", name, code, sent, stderr)
		}
	}
	if after := mustGit(t, origin, "for-each-ref"); after != before {
		t.Errorf("a refused submit pushed: origin had\n%s\nand has\n%s", before, after)
	}
}

// The baseline is the objective's and no hypothesis: its job names none and runs on the
// baseline's branch, which the submit pushes like any other.
func TestJobSubmitWithNoHypothesisIsTheBaselinesOnItsBranch(t *testing.T) {
	clone, origin := pushedClone(t)
	writeJob(t, filepath.Join(clone, "job.yaml"), "job:\n  image: foo\n")
	mustGit(t, clone, "commit", "-q", "-am", "the baseline's spec")
	code, stderr, sent := submit(t, "job.yaml")
	if code != 1 || sent != nil || !strings.Contains(stderr, "git checkout -b hl/pe-1/baseline") || !strings.Contains(stderr, "--hypothesis ID") {
		t.Fatalf("off the baseline's branch: exit code = %d, sent = %v, stderr = %s, want it refused naming the branch and --hypothesis", code, sent, stderr)
	}
	mustGit(t, clone, "checkout", "-q", "-b", "hl/pe-1/baseline")
	code, stderr, sent = submit(t, "job.yaml")
	if code != 0 || sent["branch"] != "hl/pe-1/baseline" || sent["hypothesis_id"] != nil || sent["theory"] != nil {
		t.Fatalf("on the baseline's branch: exit code = %d, sent = %v, stderr = %s, want it sent with no hypothesis and no theory", code, sent, stderr)
	}
	if pushed := mustGit(t, origin, "rev-parse", "refs/heads/hl/pe-1/baseline"); pushed != mustGit(t, clone, "rev-parse", "HEAD") {
		t.Fatalf("origin has the baseline's branch at %s, want the submitted commit", pushed)
	}
}

func TestJobSubmitRefusesDirtyTree(t *testing.T) {
	clone, _ := pushedClone(t)
	writeJob(t, filepath.Join(clone, "job.yaml"), "metadata:\n  hypothesis_id: h2\njob:\n  image: foo\n")
	code, stderr, sent := submit(t, "job.yaml")
	if code == 0 || sent != nil || !strings.Contains(stderr, "uncommitted changes") {
		t.Fatalf("exit code = %d, sent = %v, stderr = %s", code, sent, stderr)
	}
}

func TestJobSubmitKeepsExplicitCodeRef(t *testing.T) {
	clone, _ := pushedClone(t)
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "local only")
	explicit := "git://elsewhere@" + strings.Repeat("b", 40)
	writeJob(t, filepath.Join(clone, "job.yaml"), "metadata:\n  code_ref: "+explicit+"\njob:\n  image: foo\n")
	code, stderr, sent := submit(t, "job.yaml")
	if code != 0 || sent["code_ref"] != explicit {
		t.Fatalf("exit code = %d, code_ref = %v, stderr = %s", code, sent["code_ref"], stderr)
	}
}

func TestJobSubmitOutsideRepoWithoutCodeRefFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	t.Chdir(dir)
	writeJob(t, "job.yaml", "metadata:\n  hypothesis_id: h1\njob:\n  image: foo\n")
	code, stderr, _ := submit(t, "job.yaml")
	if code != 1 || !strings.Contains(stderr, "submit from your clone") {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
}

func TestJobSubmitFlagsOverrideTheCommittedSpecWithoutACommit(t *testing.T) {
	clone, origin := pushedClone(t)
	mustGit(t, clone, "checkout", "-q", "-b", "hl/pe-flag/jane/idea-abc123")
	var sent map[string]any
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-1"})
	}))
	var stdout, stderr bytes.Buffer
	args := []string{"job", "submit", "job.yaml", "--agent", "jane", "--hypothesis", "hyp-9", "--theory", "wider wins",
		"--env", "LR=0.1", "--env", "NOTE=a=b", "--platform-experiment", "pe-flag",
		"--estimated-duration-hours", "0.05", "--capacity-tier", "burst"}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	metadata := sent["metadata"].(map[string]any)
	if metadata["hypothesis_id"] != "hyp-9" || metadata["theory"] != "wider wins" || metadata["platform_experiment_id"] != "pe-flag" {
		t.Fatalf("metadata = %+v", metadata)
	}
	// A researcher refused for quota wrote five copies of the spec, each committed and pushed,
	// to change these two fields.
	if metadata["estimated_duration_hours"] != 0.05 || metadata["capacity_tier"] != "burst" {
		t.Fatalf("metadata = %+v, want the estimate and the tier of the flags", metadata)
	}
	if want := origin + "@" + mustGit(t, clone, "rev-parse", "HEAD"); metadata["code_ref"] != want {
		t.Fatalf("code_ref = %v, want %s", metadata["code_ref"], want)
	}
	env := sent["job"].(map[string]any)["env"].(map[string]any)
	if env["LR"] != "0.1" || env["NOTE"] != "a=b" {
		t.Fatalf("env = %+v", env)
	}
	if dirty := mustGit(t, clone, "status", "--porcelain"); dirty != "" {
		t.Fatalf("submit changed the clone: %s", dirty)
	}
}

func TestJobSubmitRejectsMalformedEnv(t *testing.T) {
	pushedClone(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "submit", "job.yaml", "--agent", "jane", "--env", "LR"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2, stderr = %s", code, stderr.String())
	}
	if code := run([]string{"job", "submit", "job.yaml", "--agent", "jane", "--estimated-duration-hours", "0"}, &stdout, &stderr); code != 2 {
		t.Fatalf("an estimate of 0: exit code = %d, want 2, stderr = %s", code, stderr.String())
	}
}

func TestJobSubmitSendsSecretEnvFromTheEnvironmentByName(t *testing.T) {
	pushedClone(t)
	var sent map[string]any
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-1"})
	}))
	t.Setenv("HF_TOKEN", "the-secret-value")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "submit", "job.yaml", "--agent", "jane", "--platform-experiment", "pe-1", "--secret-env", "HF_TOKEN"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	secret, _ := sent["job"].(map[string]any)["secret_env"].(map[string]any)
	if len(secret) != 1 || secret["HF_TOKEN"] != "the-secret-value" {
		t.Fatalf("job.secret_env = %+v, want HF_TOKEN from the environment", secret)
	}
	if printed := stdout.String() + stderr.String(); strings.Contains(printed, "the-secret-value") {
		t.Fatalf("submit printed the secret: %s", printed)
	}

	stderr.Reset()
	if code := run([]string{"job", "submit", "job.yaml", "--agent", "jane", "--secret-env", "HL_NOT_SET_ANYWHERE"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "$HL_NOT_SET_ANYWHERE is not set") {
		t.Fatalf("an unset variable: exit code = %d, want 2, stderr = %s", code, stderr.String())
	}
}

func TestJobSubmitRefusesSecretEnvInTheSpecFile(t *testing.T) {
	clone, _ := pushedClone(t)
	asked := false
	setAPIURL(t, withServer(t, func(http.ResponseWriter, *http.Request) { asked = true }))
	writeJob(t, filepath.Join(clone, "secret.yaml"), "metadata:\n  hypothesis_id: h1\njob:\n  image: foo\n  secret_env:\n    HF_TOKEN: the-secret-value\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "submit", "secret.yaml", "--agent", "jane"}, &stdout, &stderr)
	if code != 2 || asked || !strings.Contains(stderr.String(), "FILE carries job.secret_env") || strings.Contains(stderr.String(), "the-secret-value") {
		t.Fatalf("exit code = %d, sent = %v, stderr = %s, want it refused before anything is sent", code, asked, stderr.String())
	}
}

// A job runs a commit of the experiment's repository: a directory that is a repository of its
// own and no clone has nothing a job could be given.
func TestJobSubmitWithNoOriginSaysToWorkInTheClone(t *testing.T) {
	clone, _ := pushedClone(t)
	mustGit(t, clone, "remote", "remove", "origin")
	code, stderr, sent := submit(t, "job.yaml")
	if code != 1 || sent != nil || !strings.Contains(stderr, "work in your clone of it") {
		t.Fatalf("exit code = %d, sent = %v, stderr = %s, want it refused naming the clone", code, sent, stderr)
	}
}

func TestHLCloneGivesUpNamingTheCauseAfterItsTries(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "agents", "coordinator", "experiments", "hl-clone"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	missing := "file://" + filepath.Join(t.TempDir(), "no-such-repository")
	cmd := exec.Command("sh", script, missing+"@"+strings.Repeat("a", 40), filepath.Join(t.TempDir(), "code"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if err == nil || stdout.Len() != 0 || len(lines) != 4 || !strings.HasPrefix(lines[0], "hl-clone: try 1 of 4") ||
		!strings.HasPrefix(lines[3], "hl-clone: could not fetch the code: 4 tries") || !strings.Contains(lines[3], "no-such-repository") {
		t.Fatalf("hl-clone: %v, stdout %q, stderr:\n%s", err, stdout.String(), stderr.String())
	}
}

// hl-clone is what a job and an agent take a code_ref's commit back with.
func TestHLCloneChecksOutTheCommitOfACodeRef(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "agents", "coordinator", "experiments", "hl-clone"))
	if err != nil {
		t.Fatal(err)
	}
	clone, origin := pushedClone(t)
	sha := mustGit(t, clone, "rev-parse", "HEAD")
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "a later commit")
	mustGit(t, clone, "push", "-q", "origin", "HEAD")
	out := filepath.Join(t.TempDir(), "code")
	if printed, err := exec.Command("sh", script, "file://"+origin+"@"+sha, out).CombinedOutput(); err != nil || strings.TrimSpace(string(printed)) != out {
		t.Fatalf("hl-clone: %v: %s", err, printed)
	}
	if got := mustGit(t, out, "rev-parse", "HEAD"); got != sha {
		t.Fatalf("checked out %s, want %s", got, sha)
	}
	if _, err := os.Stat(filepath.Join(out, "job.yaml")); err != nil {
		t.Fatalf("the commit's files are not there: %v", err)
	}
}
