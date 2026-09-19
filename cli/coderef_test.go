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
