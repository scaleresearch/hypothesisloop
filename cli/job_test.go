package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// statusServer answers GET /experiments/ID with the job at status[ID], and a watch of one job with
// its status event, holding the stream open after it as the platform does.
func statusServer(t *testing.T, status map[string]string) {
	t.Helper()
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/experiments/")
		if r.URL.Path != "/watch" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": status[id]})
			return
		}
		id = r.URL.Query().Get("experiment_id")
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(sum[:]))
		event := fmt.Sprintf(`{"cursor":1,"position":1,"kind":"experiment.status","subject":%q,"value":%q}`, id, status[id])
		buf.Write(append([]byte{0x81, byte(len(event))}, event...))
		_ = buf.Flush()
		_, _ = io.Copy(io.Discard, conn)
	}))
}

// A coordinator waited on jobs with sleep and hl job list: hl job wait returns once each has
// ended and prints it as a listing does.
func TestJobWaitReturnsOnceEveryJobEnded(t *testing.T) {
	statusServer(t, map[string]string{"job-1": "COMPLETED", "job-2": "FAILED"})
	code, stdout, stderr := hl(t, "job", "wait", "job-1", "job-2")
	if code != 0 || !strings.Contains(stdout, `"id":"job-1"`) || !strings.Contains(stdout, `"status":"FAILED"`) || len(lines(stdout)) != 2 {
		t.Errorf("exit %d, stdout %q, stderr %q, want 0 and one line per job", code, stdout, stderr)
	}
}

func TestJobWaitTimesOutWithTheJobsAsTheyStand(t *testing.T) {
	statusServer(t, map[string]string{"job-1": "COMPLETED", "job-2": "RUNNING"})
	code, stdout, stderr := hl(t, "job", "wait", "job-1", "job-2", "--timeout", "0.5")
	if code != 124 || !strings.Contains(stdout, `"status":"RUNNING"`) || len(lines(stdout)) != 2 || !strings.Contains(stderr, "timed out") {
		t.Errorf("exit %d, stdout %q, stderr %q, want 124, both jobs and why", code, stdout, stderr)
	}
}

func writeJobFile(t *testing.T) string {
	t.Helper()
	file := t.TempDir() + "/job.yaml"
	if err := os.WriteFile(file, []byte("metadata:\n  code_ref: repo@abc\n  platform_experiment_id: pe-1\n  hypothesis_id: hyp-1\njob:\n  image: foo:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// A submit the platform stored after hl stopped waiting was sent again and made a second job.
// hl names a submit by all it sends, so the same one sent twice carries one key, and a submit
// that differs in any part, or says --repeat, carries another.
func TestASubmitCarriesAKeyThatOnlyTheSameSubmitRepeats(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "job-1", "status": "QUEUED"} })
	file := writeJobFile(t)
	keyOf := func(args ...string) string {
		t.Helper()
		*got = nil
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"job", "submit", "--agent", "jane", file}, args...), &stdout, &stderr); code != 0 {
			t.Fatalf("hl job submit %v: exit %d, stderr %s", args, code, stderr.String())
		}
		key, _ := (*got)[0].Body["submission_key"].(string)
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) {
			t.Fatalf("hl job submit %v: submission_key %q, want 64 hex digits", args, key)
		}
		return key
	}
	first := keyOf()
	if again := keyOf(); again != first {
		t.Errorf("the same submit sent twice carries %s and %s, want one key", first, again)
	}
	if keyOf("--repeat", "1") != first {
		t.Error("--repeat 1 is the first job of the request, want its key")
	}
	seen := map[string]string{first: "the plain submit"}
	for _, args := range [][]string{{"--repeat", "2"}, {"--repeat", "3"}, {"--env", "SEED=1"}, {"--hypothesis", "hyp-2"}, {"--theory", "t"}, {"--estimated-duration-hours", "0.5"}, {"--capacity-tier", "burst"}} {
		key := keyOf(args...)
		if other, taken := seen[key]; taken {
			t.Errorf("%v carries the key of %s, want a key of its own", args, other)
		}
		seen[key] = strings.Join(args, " ")
	}
	t.Setenv("HF_TOKEN", "one")
	one := keyOf("--secret-env", "HF_TOKEN")
	t.Setenv("HF_TOKEN", "two")
	if keyOf("--secret-env", "HF_TOKEN") == one {
		t.Error("a submit with another secret value carries the same key, want another")
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "submit", "--agent", "jane", file, "--repeat", "0"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "--repeat wants 1 or more") {
		t.Errorf("--repeat 0: exit %d, stderr %q, want 2 and why", code, stderr.String())
	}
}

// A job a submit created is QUEUED in its answer. Any other status is the job an identical submit
// created before: hl says nothing was submitted, and fails when that job has ended, since no
// result will arrive for the agent to wait on.
func TestASubmitAnsweredWithAnEarlierJobSaysNothingWasSubmitted(t *testing.T) {
	status := ""
	recordingServer(t, func(int, recorded) any { return map[string]any{"id": "job-1", "status": status} })
	file := writeJobFile(t)
	for _, tc := range []struct {
		status string
		args   []string
		code   int
		want   string
	}{
		{"RUNNING", nil, 0, "hl: job submit: an identical submit already created job-1, which is RUNNING, and nothing more was submitted. --repeat 2 submits the same request as another job\n"},
		{"EVICTED", nil, 1, "hl: job submit: nothing was submitted: an identical submit already created job-1, which ended EVICTED. --repeat 2 submits the same request as another job\n"},
		{"COMPLETED", []string{"--repeat", "2"}, 1, "hl: job submit: nothing was submitted: an identical submit already created job-1, which ended COMPLETED. --repeat 3 submits the same request as another job\n"},
	} {
		status = tc.status
		var stdout, stderr bytes.Buffer
		code := run(append([]string{"job", "submit", "--agent", "jane", file}, tc.args...), &stdout, &stderr)
		if code != tc.code || stderr.String() != tc.want || stdout.String() != `{"id":"job-1","status":"`+tc.status+`"}`+"\n" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want %d and %q", tc.status, code, stdout.String(), stderr.String(), tc.code, tc.want)
		}
	}
}

// Agents ran hl job summary ID to read a summary and were refused: without --text it prints it.
func TestJobSummaryWithoutTextPrintsIt(t *testing.T) {
	got := recordingServer(t, func(_ int, r recorded) any {
		return map[string]any{"id": "job-1", "summary": "bf16 won by 3%"}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"job", "summary", "job-1"}, &stdout, &stderr); code != 0 || stdout.String() != "bf16 won by 3%\n" {
		t.Errorf("exit %d, stdout %q, stderr %q, want the summary", code, stdout.String(), stderr.String())
	}
	if len(*got) != 1 || (*got)[0].Method != "GET" || (*got)[0].Path != "/experiments/job-1" {
		t.Errorf("asked %+v, want one read of the job", *got)
	}
}
