package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An environment is built ahead of its job from the commit the job will run, pushed as a submit
// pushes it, on the image the job spec names.
func TestEnvBuildSendsTheCommitAndTheImageOfTheJobSpec(t *testing.T) {
	clone, origin := pushedClone(t)
	writeJob(t, filepath.Join(clone, ".hypothesisloop-Dockerfile"), "ARG BASE\nFROM ${BASE}\n")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-q", "-m", "the environment")
	var sent map[string]any
	var path string
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "k1", "state": "building", "base": "registry.test/job:1", "message": "the platform builds it"})
	}))
	var stdout, stderr bytes.Buffer
	code := run([]string{"env", "build", "--platform-experiment", "pe-1", "job.yaml"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	head := mustGit(t, clone, "rev-parse", "HEAD")
	if path != "POST /environments" || sent["code_ref"] != origin+"@"+head || sent["image"] != "foo" || sent["platform_experiment_id"] != "pe-1" {
		t.Errorf("sent %s %v, want the commit, the spec's image and the platform experiment", path, sent)
	}
	if pushed := mustGit(t, origin, "rev-parse", "refs/heads/"+testBranch); pushed != head {
		t.Errorf("origin has %s at %s, want the commit built", testBranch, pushed)
	}
	if !strings.Contains(stdout.String(), `"state":"building"`) {
		t.Errorf("stdout %q, want the environment", stdout.String())
	}
}

// Run 8's baseline ran hl env build with no FILE and was refused for want of an image that its
// repository's job spec names.
func TestEnvBuildReadsTheRepositorysJobSpecWhenNoFileIsNamed(t *testing.T) {
	clone, _ := pushedClone(t)
	if err := os.MkdirAll(filepath.Join(clone, ".hypothesisloop"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJob(t, filepath.Join(clone, ".hypothesisloop", "job.yaml"), "job:\n  image: from-the-spec\n")
	mustGit(t, clone, "add", ".")
	mustGit(t, clone, "commit", "-q", "-m", "the job spec")
	if err := os.MkdirAll(filepath.Join(clone, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(clone, "sub"))
	var sent map[string]any
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "k1", "state": "building"})
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "build", "--platform-experiment", "pe-1", "--hypothesis", "h1"}, &stdout, &stderr); code != 0 || sent["image"] != "from-the-spec" {
		t.Fatalf("exit %d, sent %v, stderr %s, want the image of .hypothesisloop/job.yaml", code, sent, stderr.String())
	}
}

func TestEnvBuildWantsAnImage(t *testing.T) {
	pushedClone(t)
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("sent %s %s with no image", r.Method, r.URL.Path)
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "build", "--platform-experiment", "pe-1", "--hypothesis", "h1"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--image") {
		t.Errorf("exit %d, stderr %q, want a misuse naming --image", code, stderr.String())
	}
}

func TestEnvBuildOfAFailedFileExitsNonZeroWithItsOutput(t *testing.T) {
	pushedClone(t)
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "k1", "state": "failed", "fault": "file", "message": "E: Unable to locate package rustc"})
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "build", "--platform-experiment", "pe-1", "--image", "python", "--hypothesis", "h1"}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "rustc") {
		t.Errorf("exit %d, stdout %q, want 1 with the build's output", code, stdout.String())
	}
}
