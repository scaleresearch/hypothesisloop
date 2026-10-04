package main

import (
	"bytes"
	"encoding/json"
	"net/http"
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
