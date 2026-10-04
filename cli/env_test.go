package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
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

// checkServer answers a check's submit, then its status and logs from the given polls, one per
// read, the last repeated. It returns the body the submit sent and the paths cancelled.
type checkReply struct {
	record map[string]any
	logs   []string
}

func checkServer(t *testing.T, polls []checkReply) (*map[string]any, *[]string) {
	t.Helper()
	var sent map[string]any
	var cancelled []string
	statusReads, logReads := 0, 0
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/environments":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"reason": "no_environment_file", "message": "commit holds no .hypothesisloop/Dockerfile"})
		case r.Method == "POST" && r.URL.Path == "/experiments":
			_ = json.NewDecoder(r.Body).Decode(&sent)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-c", "status": "QUEUED", "check": true})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/cancel"):
			cancelled = append(cancelled, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-c", "status": "CANCELLED"})
		case r.URL.Path == "/experiments/job-c/logs":
			_ = json.NewEncoder(w).Encode(polls[min(logReads, len(polls)-1)].logs)
			logReads++
		case r.URL.Path == "/experiments/job-c":
			_ = json.NewEncoder(w).Encode(polls[min(statusReads, len(polls)-1)].record)
			statusReads++
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	return &sent, &cancelled
}

func fastCheckPoll(t *testing.T) {
	t.Helper()
	old := checkPoll
	checkPoll = time.Millisecond
	t.Cleanup(func() { checkPoll = old })
}

// R2 had no cargo on its machine: a check runs the command in the environment, from the commit,
// with the spec's image and resources and no hypothesis or theory.
func TestEnvRunSubmitsACheckOfTheCommandOnTheSpecsMachine(t *testing.T) {
	clone, origin := pushedClone(t)
	writeJob(t, filepath.Join(clone, "job.yaml"), "metadata:\n  hypothesis_id: h1\n  theory: faster\n  estimated_duration_hours: 2\njob:\n  image: foo\n  command: [\"bash\", \"-c\", \"make bench\"]\n  cpu: 4\n  memory: 8Gi\n  env:\n    A: \"1\"\n")
	mustGit(t, clone, "commit", "-q", "-am", "spec")
	fastCheckPoll(t)
	sent, _ := checkServer(t, []checkReply{{record: map[string]any{"status": "COMPLETED"}, logs: []string{"built"}}})
	var stdout, stderr bytes.Buffer
	code := run([]string{"env", "run", "--agent", "ag-1", "--platform-experiment", "pe-1", "--cpu", "2", "--env", "B=2", "job.yaml", "--", "cargo", "test", "it's"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	metadata, _ := (*sent)["metadata"].(map[string]any)
	job, _ := (*sent)["job"].(map[string]any)
	head := mustGit(t, clone, "rev-parse", "HEAD")
	if metadata["check"] != true || metadata["hypothesis_id"] != nil || metadata["theory"] != nil || metadata["estimated_duration_hours"] != nil ||
		metadata["code_ref"] != origin+"@"+head || metadata["agent_id"] != "ag-1" || (*sent)["submission_key"] == nil {
		t.Errorf("metadata %v, want a check of HEAD with no hypothesis, theory or the job's estimate", metadata)
	}
	command, _ := job["command"].([]any)
	if len(command) != 3 || command[0] != "bash" || command[1] != "-c" || command[2] != `dir=$(hl-clone) && cd "$dir" && cargo test 'it'\''s'` {
		t.Errorf("command %q", command)
	}
	env, _ := job["env"].(map[string]any)
	if job["image"] != "foo" || job["cpu"] != 2.0 || job["memory"] != "8Gi" || env["A"] != "1" || env["B"] != "2" {
		t.Errorf("job %v, want the spec's image and memory with the overrides", job)
	}
	if !strings.Contains(stdout.String(), `"id":"job-c"`) || !strings.HasSuffix(stdout.String(), "built\n") {
		t.Errorf("stdout %q", stdout.String())
	}
}

func TestEnvRunTakesOneArgumentAsAShellLine(t *testing.T) {
	if got := checkCommand([]string{"cargo build && ./x"}); got[2] != `dir=$(hl-clone) && cd "$dir" && cargo build && ./x` {
		t.Errorf("command %q", got)
	}
}

func TestEnvRunWantsACommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "run", "--agent", "ag-1", "job.yaml"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

// The logs endpoint answers a tail: each line is printed once however often the tail repeats it.
func TestEnvRunPrintsEachLineOnceAndExitsWithTheCommandsCode(t *testing.T) {
	pushedClone(t)
	fastCheckPoll(t)
	checkServer(t, []checkReply{
		{record: map[string]any{"status": "QUEUED", "not_admitted_reason": "environment_building", "not_admitted_message": "building"}},
		{record: map[string]any{"status": "RUNNING", "ran_on": map[string]any{"node": "n1"}}, logs: []string{"a", "b"}},
		{record: map[string]any{"status": "RUNNING", "ran_on": map[string]any{"node": "n1"}}, logs: []string{"b", "c"}},
		{record: map[string]any{"status": "FAILED", "eviction_reason": "container_failed: container exited with code 3"}, logs: []string{"b", "c", "error[E0425]", "hl-exec: exit-code=101"}},
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"env", "run", "--agent", "ag-1", "--platform-experiment", "pe-1", "job.yaml", "--", "cargo build"}, &stdout, &stderr)
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")[1:]
	if strings.Join(lines, ",") != "a,b,c,error[E0425],hl-exec: exit-code=101" {
		t.Errorf("printed %q", lines)
	}
	if code != 101 {
		t.Errorf("exit %d, want the command's 101", code)
	}
	for _, said := range []string{"QUEUED environment_building: building", "RUNNING on node n1", "FAILED: container_failed"} {
		if !strings.Contains(stderr.String(), said) {
			t.Errorf("stderr %q, want %q", stderr.String(), said)
		}
	}
}

func TestEnvRunTakesTheExitCodeTheRuntimeReported(t *testing.T) {
	rec := checkRecord{Status: "FAILED", EvictionReason: "container_failed: container exited with code 2"}
	if code := checkExit(rec, nil); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if code := checkExit(checkRecord{Status: "EVICTED", EvictionReason: "job_too_long"}, nil); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
}

func TestEnvRunCancelsTheCheckWhenInterrupted(t *testing.T) {
	pushedClone(t)
	_, cancelled := checkServer(t, []checkReply{{record: map[string]any{"status": "RUNNING"}}})
	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	var stdout, stderr bytes.Buffer
	t.Setenv("HL_TOKEN", "test-token")
	c := apiclient.New(os.Getenv("API_URL"), "test-token")
	code := runCheck(c, []string{"--agent", "ag-1", "--platform-experiment", "pe-1", "job.yaml", "--", "sleep 100"}, &stdout, &stderr, signals)
	if code != 130 || len(*cancelled) != 1 || (*cancelled)[0] != "/experiments/job-c/cancel" {
		t.Errorf("exit %d, cancelled %v, stderr %s", code, *cancelled, stderr.String())
	}
}

// Run 10's baseline ran hl env run three times to see whether its build was done, and one made a
// job that was rejected once the build failed. A check waits for its environment, makes no job
// while it builds, and none once its file failed.
func TestEnvRunWaitsForItsEnvironmentAndMakesNoCheckOfAFailedOne(t *testing.T) {
	for name, final := range map[string]map[string]string{
		"failed": {"key": "k1", "state": "failed", "fault": "file", "message": "error: unexpected argument '--locked=false'"},
		"ready":  {"key": "k1", "state": "ready", "image": "registry.test/env@sha256:1", "message": "built"},
	} {
		pushedClone(t)
		fastCheckPoll(t)
		var asked []string
		checks := 0
		reads := 0
		setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "POST" && r.URL.Path == "/environments":
				asked = append(asked, "POST")
				_ = json.NewEncoder(w).Encode(map[string]string{"key": "k1", "state": "building", "message": "It builds on cluster quietbox"})
			case r.Method == "GET" && r.URL.Path == "/environments":
				if r.URL.Query().Get("image") != "foo" || r.URL.Query().Get("code_ref") == "" {
					t.Errorf("%s: read %s, want the check's commit and image", name, r.URL.RawQuery)
				}
				if reads++; reads < 3 {
					_ = json.NewEncoder(w).Encode(map[string]string{"key": "k1", "state": "building", "message": "It builds on cluster quietbox"})
					return
				}
				_ = json.NewEncoder(w).Encode(final)
			case r.Method == "POST" && r.URL.Path == "/experiments":
				checks++
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "job-c", "status": "QUEUED", "check": true})
			case r.URL.Path == "/experiments/job-c/logs":
				_ = json.NewEncoder(w).Encode([]string{})
			case r.URL.Path == "/experiments/job-c":
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "COMPLETED"})
			default:
				t.Errorf("%s: unexpected %s %s", name, r.Method, r.URL.Path)
			}
		}))
		var stdout, stderr bytes.Buffer
		code := run([]string{"env", "run", "--agent", "ag-1", "--platform-experiment", "pe-1", "job.yaml", "--", "true"}, &stdout, &stderr)
		if len(asked) != 1 || strings.Count(stderr.String(), "no check is made until the environment is built") != 1 {
			t.Errorf("%s: asked %v, stderr %q, want one build asked and the wait said once", name, asked, stderr.String())
		}
		switch name {
		case "failed":
			if code != 1 || checks != 0 || !strings.Contains(stdout.String(), "--locked=false") {
				t.Errorf("failed: exit %d with %d checks, stdout %q, want 1, no check and the build's output", code, checks, stdout.String())
			}
		case "ready":
			if code != 0 || checks != 1 {
				t.Errorf("ready: exit %d with %d checks, want the check made once built", code, checks)
			}
		}
	}
}

// The state of an environment is read without building it.
func TestEnvStatusReadsTheEnvironmentAndBuildsNothing(t *testing.T) {
	pushedClone(t)
	var path string
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.Method + " " + r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "k1", "state": "failed", "fault": "file", "message": "E: Unable to locate package rustc"})
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"env", "status", "--platform-experiment", "pe-1", "--image", "python", "--hypothesis", "h1"}, &stdout, &stderr); code != 1 || path != "GET /environments" || !strings.Contains(stdout.String(), "rustc") {
		t.Errorf("exit %d after %s, stdout %q, want 1 from a read with the build's output", code, path, stdout.String())
	}
}
