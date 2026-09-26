package main

import (
	"bytes"
	"encoding/json"
	"io"
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

// pushedClone returns a clone whose HEAD is committed and pushed to a local bare origin.
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
	t.Chdir(clone)
	return clone, origin
}

func writeJob(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// submit runs `hl job submit` on path and returns the exit code, stderr and the code_ref the API
// received (nil when nothing was sent).
func submit(t *testing.T, path string) (int, string, any) {
	t.Helper()
	var sent any
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sent = body["metadata"].(map[string]any)["code_ref"]
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-1"})
	}))
	var stdout, stderr bytes.Buffer
	code := run([]string{"job", "submit", "--agent", "jane", path}, &stdout, &stderr)
	return code, stderr.String(), sent
}

func TestJobSubmitFillsCodeRefFromPushedHead(t *testing.T) {
	clone, origin := pushedClone(t)
	code, stderr, sent := submit(t, "job.yaml")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if want := origin + "@" + mustGit(t, clone, "rev-parse", "HEAD"); sent != want {
		t.Fatalf("code_ref = %v, want %s", sent, want)
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

func TestJobSubmitRefusesUnpushedHead(t *testing.T) {
	clone, _ := pushedClone(t)
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "local only")
	code, stderr, sent := submit(t, "job.yaml")
	if code == 0 || sent != nil || !strings.Contains(stderr, "not on origin") {
		t.Fatalf("exit code = %d, sent = %v, stderr = %s", code, sent, stderr)
	}
}

func TestJobSubmitKeepsExplicitCodeRef(t *testing.T) {
	clone, _ := pushedClone(t)
	mustGit(t, clone, "commit", "-q", "--allow-empty", "-m", "local only")
	explicit := "git://elsewhere@" + strings.Repeat("b", 40)
	writeJob(t, filepath.Join(clone, "job.yaml"), "metadata:\n  code_ref: "+explicit+"\njob:\n  image: foo\n")
	code, stderr, sent := submit(t, "job.yaml")
	if code != 0 || sent != explicit {
		t.Fatalf("exit code = %d, code_ref = %v, stderr = %s", code, sent, stderr)
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
	var sent map[string]any
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-1"})
	}))
	var stdout, stderr bytes.Buffer
	args := []string{"job", "submit", "job.yaml", "--agent", "jane", "--hypothesis", "hyp-9", "--theory", "wider wins",
		"--env", "LR=0.1", "--env", "NOTE=a=b", "--platform-experiment", "pe-flag"}
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	metadata := sent["metadata"].(map[string]any)
	if metadata["hypothesis_id"] != "hyp-9" || metadata["theory"] != "wider wins" || metadata["platform_experiment_id"] != "pe-flag" {
		t.Fatalf("metadata = %+v", metadata)
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
	if code := run([]string{"job", "submit", "job.yaml", "--agent", "jane", "--secret-env", "HF_TOKEN"}, &stdout, &stderr); code != 0 {
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

// codePlatform is the platform's code routes kept in memory, and what a job submit sent.
type codePlatform struct {
	stored map[string][]byte
	sent   any
}

func (p *codePlatform) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.Contains(r.URL.Path, "/code/") && r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		p.stored[r.URL.Path] = body
		_, _ = w.Write([]byte(`{}`))
	case strings.Contains(r.URL.Path, "/code/"):
		body, found := p.stored[r.URL.Path]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	default:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.sent = body["metadata"].(map[string]any)["code_ref"]
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "job-1"})
	}
}

// localRepository is a repository with one commit and no origin: what an agent of a platform
// experiment without a repository works in.
func localRepository(t *testing.T) (dir, api string, platform *codePlatform) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir = t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	writeJob(t, filepath.Join(dir, "job.yaml"), "metadata:\n  hypothesis_id: h1\n  platform_experiment_id: pe-1\njob:\n  image: foo\n")
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-q", "-m", "init")
	t.Chdir(dir)
	platform = &codePlatform{stored: map[string][]byte{}}
	api = withServer(t, platform.serve)
	setAPIURL(t, api)
	return dir, api, platform
}

// An experiment with no repository had no code_ref to give a job at all.
func TestWithNoOriginAJobCarriesTheCommitThePlatformKeepsOncePushed(t *testing.T) {
	dir, api, platform := localRepository(t)
	sha := mustGit(t, dir, "rev-parse", "HEAD")
	var stdout, stderr bytes.Buffer
	submitHere := func() int {
		stdout.Reset()
		stderr.Reset()
		return run([]string{"job", "submit", "--agent", "jane", "job.yaml"}, &stdout, &stderr)
	}
	if code := submitHere(); code == 0 || platform.sent != nil || !strings.Contains(stderr.String(), "hl code push, then submit again") {
		t.Fatalf("before the push: exit %d, sent %v, stderr %s", code, platform.sent, stderr.String())
	}

	stdout.Reset()
	if code := run([]string{"code", "push", "--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code push: exit %d, %s", code, stderr.String())
	}
	ref := api + "/platform-experiments/pe-1/agents/jane/code@" + sha
	if got := strings.TrimSpace(stdout.String()); got != ref {
		t.Fatalf("code push printed %q, want %s", got, ref)
	}
	bundle := platform.stored["/platform-experiments/pe-1/agents/jane/code/"+sha]
	if !bytes.HasPrefix(bundle, []byte("# v")) || !bytes.Contains(bundle, []byte(sha+" ")) {
		t.Fatalf("stored %q, want a git bundle whose tip is HEAD", bundle[:min(len(bundle), 80)])
	}
	if code := submitHere(); code != 0 || platform.sent != ref {
		t.Fatalf("after the push: exit %d, code_ref %v, want %s; stderr %s", code, platform.sent, ref, stderr.String())
	}

	// A commit made since is not what the platform holds.
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "local only")
	platform.sent = nil
	if code := submitHere(); code == 0 || platform.sent != nil {
		t.Fatalf("an unpushed commit was submitted: exit %d, sent %v", code, platform.sent)
	}
}

const hlProcessEnv = "HL_TEST_RUN_AS_HL"

// TestHLProcess is the hl that hl-clone runs in the test below: the test binary acting as hl
// with the arguments after "--".
func TestHLProcess(t *testing.T) {
	if os.Getenv(hlProcessEnv) == "" {
		t.Skip("run by hl-clone as hl")
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(run(os.Args[i+1:], os.Stdout, os.Stderr))
		}
	}
	os.Exit(2)
}

func TestHLCloneChecksOutACommitThePlatformKeeps(t *testing.T) {
	cliDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, api, _ := localRepository(t)
	sha := mustGit(t, dir, "rev-parse", "HEAD")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"code", "push", "--agent", "jane", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code push: exit %d, %s", code, stderr.String())
	}
	// hl-clone runs the hl on its PATH: this test binary, told to act as hl.
	bin := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "hl"), []byte("#!/bin/sh\n"+hlProcessEnv+"=1 exec "+self+" -test.run=TestHLProcess -- \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join(cliDir, "..", "agents", "coordinator", "experiments", "hl-clone"))
	if err != nil {
		t.Fatal(err)
	}
	// As a job has it: the platform's address and its own token under the job's names, and a
	// code_ref written with another address for the same API.
	out := filepath.Join(t.TempDir(), "code")
	cmd := exec.Command("sh", script, "https://api.as-the-agent-saw-it.example/platform-experiments/pe-1/agents/jane/code@"+sha, out)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "API_URL=", "HL_TOKEN=",
		"HYPOTHESISLOOP_API_URL="+api, "HYPOTHESISLOOP_TOKEN=job-token")
	if printed, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(printed)) != out {
		t.Fatalf("hl-clone: %v: %s", err, printed)
	}
	if got := mustGit(t, out, "rev-parse", "HEAD"); got != sha {
		t.Fatalf("checked out %s, want %s", got, sha)
	}
	if _, err := os.Stat(filepath.Join(out, "job.yaml")); err != nil {
		t.Fatalf("the commit's files are not there: %v", err)
	}
}

func TestCodeFetchTakesOnlyACodeRefThePlatformKeeps(t *testing.T) {
	_, _, _ = localRepository(t)
	var stdout, stderr bytes.Buffer
	for _, ref := range []string{"https://github.com/acme/model@" + strings.Repeat("a", 40), "https://api.example/platform-experiments/pe-1/agents/jane/code@main"} {
		if code := run([]string{"code", "fetch", ref, filepath.Join(t.TempDir(), "x")}, &stdout, &stderr); code == 0 {
			t.Errorf("code fetch took %s", ref)
		}
	}
}
