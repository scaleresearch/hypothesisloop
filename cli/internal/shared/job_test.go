package shared

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// statusServer answers GET /experiments/ID with the job at status[ID], and a watch of one job with
// its status event, holding the stream open after it as the platform does.
func statusServer(t *testing.T, status map[string]string) {
	t.Helper()
	clitest.SetAPIURL(t, clitest.WithServer(t, func(w http.ResponseWriter, r *http.Request) {
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
	code, stdout, stderr := clitest.Output(t, JobWait, "job-1", "job-2")
	if code != 0 || !strings.Contains(stdout, `"id":"job-1"`) || !strings.Contains(stdout, `"status":"FAILED"`) || len(clitest.Lines(stdout)) != 2 {
		t.Errorf("exit %d, stdout %q, stderr %q, want 0 and one line per job", code, stdout, stderr)
	}
}

func TestJobWaitTimesOutWithTheJobsAsTheyStand(t *testing.T) {
	statusServer(t, map[string]string{"job-1": "COMPLETED", "job-2": "RUNNING"})
	code, stdout, stderr := clitest.Output(t, JobWait, "job-1", "job-2", "--timeout", "0.5")
	if code != 124 || !strings.Contains(stdout, `"status":"RUNNING"`) || len(clitest.Lines(stdout)) != 2 || !strings.Contains(stderr, "timed out") {
		t.Errorf("exit %d, stdout %q, stderr %q, want 124, both jobs and why", code, stdout, stderr)
	}
}

// Agents ran hl job summary ID to read a summary and were refused: without --text it prints it.
func TestJobSummaryWithoutTextPrintsIt(t *testing.T) {
	got := clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
		return map[string]any{"id": "job-1", "summary": "bf16 won by 3%"}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(JobSummary, []string{"job-1"}, &stdout, &stderr); code != 0 || stdout.String() != "bf16 won by 3%\n" {
		t.Errorf("exit %d, stdout %q, stderr %q, want the summary", code, stdout.String(), stderr.String())
	}
	if len(*got) != 1 || (*got)[0].Method != "GET" || (*got)[0].Path != "/experiments/job-1" {
		t.Errorf("asked %+v, want one read of the job", *got)
	}
}
