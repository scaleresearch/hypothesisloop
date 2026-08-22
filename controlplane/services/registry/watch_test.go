package registry

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// fakeEvents is the change stream reduced to the two rules the real one guarantees: replay
// returns the matching events after a cursor, in cursor order, and the live stream carries
// whatever the database announced. It applies db.EventFilter itself rather than a test-local
// approximation, so a test here fails for the same reason production would.
type fakeEvents struct {
	stored []db.Event
	live   chan db.Event
}

func newFakeEvents(stored ...db.Event) *fakeEvents {
	return &fakeEvents{stored: stored, live: make(chan db.Event, 16)}
}

func (f *fakeEvents) Replay(_ context.Context, filter db.EventFilter, since int64) ([]db.Event, error) {
	var out []db.Event
	if since <= 0 {
		return nil, nil
	}
	for _, e := range f.stored {
		if e.Cursor > since && filter.Matches(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeEvents) ListenEvents(ctx context.Context, out chan<- db.Event) error {
	for {
		select {
		case e := <-f.live:
			select {
			case out <- e:
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// lookupStore embeds Store so any method a test did not think about panics instead of quietly
// answering zero.
type lookupStore struct {
	Store
	exp *domain.Experiment
}

func (s *lookupStore) GetExperiment(context.Context, string) (*domain.Experiment, error) {
	return s.exp, nil
}

func watchServer(t *testing.T, source EventSource) *httptest.Server {
	t.Helper()
	handler := NewWatchHandler(source, &lookupStore{exp: &domain.Experiment{ID: "exp-1", PlatformExperimentID: "pe-1"}}, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	handler.Start(ctx)
	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	t.Cleanup(func() {
		server.Close()
		cancel()
	})
	return server
}

// watchClient is a WebSocket client just large enough to read what the server writes. It exists
// because the repository vendors its dependencies and no WebSocket library is vendored, so the
// server's own handshake and framing have to be exercised by something written against the same
// spec rather than assumed correct.
type watchClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialWatch(t *testing.T, server *httptest.Server, query url.Values) *watchClient {
	t.Helper()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server url: got = %v, want = nil", err)
	}
	conn, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatalf("dial: got = %v, want = nil", err)
	}
	t.Cleanup(func() { conn.Close() })
	request := "GET /watch?" + query.Encode() + " HTTP/1.1\r\n" +
		"Host: " + target.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write handshake: got = %v, want = nil", err)
	}
	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: got = %v, want = nil", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("handshake status: got = %v, want = 101 Switching Protocols", strings.TrimSpace(status))
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read handshake headers: got = %v, want = nil", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	return &watchClient{conn: conn, r: r}
}

// next returns the next event the server sent, skipping the pings that keep an idle stream alive.
func (c *watchClient) next(t *testing.T) db.Event {
	t.Helper()
	for {
		if err := c.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatalf("set read deadline: got = %v, want = nil", err)
		}
		var head [2]byte
		if _, err := io.ReadFull(c.r, head[:]); err != nil {
			t.Fatalf("read frame header: got = %v, want = nil", err)
		}
		length := int64(head[1] & 0x7F)
		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				t.Fatalf("read frame length: got = %v, want = nil", err)
			}
			length = int64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.r, ext[:]); err != nil {
				t.Fatalf("read frame length: got = %v, want = nil", err)
			}
			length = int64(binary.BigEndian.Uint64(ext[:]))
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.r, payload); err != nil {
			t.Fatalf("read frame payload: got = %v, want = nil", err)
		}
		if head[0]&0x0F != wsOpText {
			continue
		}
		var e db.Event
		if err := json.Unmarshal(payload, &e); err != nil {
			t.Fatalf("decode event %q: got = %v, want = nil", payload, err)
		}
		return e
	}
}

func statusEvent(subject, value, agentID string, cursor int64) db.Event {
	return db.Event{Kind: db.EventExperimentStatus, Subject: subject, Value: value,
		PlatformExperimentID: "pe-1", AgentID: agentID, Cursor: cursor}
}

// A dropped connection must cost a client a delay and never a fact. An agent that reconnects and
// silently skips the transition it was disconnected across would go back to polling full state,
// which is the exact failure this stream exists to end — so replay has to return every event
// after the cursor and start the live stream from where it stopped.
func TestAReconnectWithTheLastCursorReplaysExactlyTheEventsMissedWhileDisconnected(t *testing.T) {
	source := newFakeEvents(
		statusEvent("exp-1", "QUEUED", "agent-a", 100),
		statusEvent("exp-1", "SUBMITTED", "agent-a", 200),
		statusEvent("exp-1", "RUNNING", "agent-a", 300),
	)
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"since":                  {"100"},
	})

	for _, want := range []string{"SUBMITTED", "RUNNING"} {
		if got := client.next(t).Value; got != want {
			t.Errorf("replayed status: got = %v, want = %v", got, want)
		}
	}
	source.live <- statusEvent("exp-1", "COMPLETED", "agent-a", 400)
	if got := client.next(t).Value; got != "COMPLETED" {
		t.Errorf("live status after replay: got = %v, want = %v", got, "COMPLETED")
	}
}

// The subscription is opened before the replay query runs, so an event committed between the two
// is delivered twice unless the cursor removes it. A client that saw the same transition twice
// would count two attempts where there was one — and the duplicate is invisible in the transcript
// unless the server refuses to send it.
func TestAnEventAlreadyReplayedIsNotSentAgainWhenTheLiveStreamRepeatsIt(t *testing.T) {
	source := newFakeEvents(
		statusEvent("exp-1", "SUBMITTED", "agent-a", 200),
		statusEvent("exp-1", "RUNNING", "agent-a", 300),
	)
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"since":                  {"100"},
	})

	if got := client.next(t).Value; got != "SUBMITTED" {
		t.Fatalf("first replayed status: got = %v, want = %v", got, "SUBMITTED")
	}
	if got := client.next(t).Value; got != "RUNNING" {
		t.Fatalf("second replayed status: got = %v, want = %v", got, "RUNNING")
	}
	// The same transition the replay already carried, announced live as it would be if the
	// commit landed while the replay was in flight.
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 300)
	source.live <- statusEvent("exp-1", "COMPLETED", "agent-a", 400)
	if got := client.next(t).Value; got != "COMPLETED" {
		t.Errorf("next status after the repeated one: got = %v, want = %v", got, "COMPLETED")
	}
}

// A subscription naming kinds is a promise about what the stream contains. Delivering a kind that
// was not asked for makes an --until condition fire on an event the caller never meant to wait
// for, so the filter has to hold on the wire and not merely in the caller's own reading of it.
func TestASubscriptionNamingKindsReceivesOnlyThoseKinds(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"kinds":                  {"experiment.status"},
	})

	source.live <- db.Event{Kind: db.EventHypothesisNew, Subject: "hyp-1",
		PlatformExperimentID: "pe-1", Cursor: 100}
	source.live <- db.Event{Kind: db.EventCommentNew, Subject: "hyp-1",
		PlatformExperimentID: "pe-1", Cursor: 150}
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 200)

	got := client.next(t)
	if got.Kind != db.EventExperimentStatus {
		t.Errorf("first delivered kind: got = %v, want = %v", got.Kind, db.EventExperimentStatus)
	}
	if got.Value != "RUNNING" {
		t.Errorf("first delivered value: got = %v, want = %v", got.Value, "RUNNING")
	}
}

// An agent subscribes to its own jobs plus the shared pool, which is one filter doing two things:
// it must hide another agent's job events and must not hide pool activity that happens to carry
// someone else's name. Getting this backwards either leaks a competitor's job timeline or hides
// the notebook the whole platform is organised around.
func TestAnAgentScopedSubscriptionHidesAnotherAgentsJobsAndKeepsPoolActivity(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"agent":                  {"agent-a"},
	})

	source.live <- statusEvent("exp-2", "RUNNING", "agent-b", 100)
	source.live <- db.Event{Kind: db.EventHypothesisNew, Subject: "hyp-1", Value: "agent",
		PlatformExperimentID: "pe-1", AgentID: "agent-b", Cursor: 200}
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 300)

	if got := client.next(t); got.Kind != db.EventHypothesisNew {
		t.Errorf("first delivered kind: got = %v, want = %v", got.Kind, db.EventHypothesisNew)
	}
	if got := client.next(t); got.Subject != "exp-1" {
		t.Errorf("second delivered subject: got = %v, want = %v", got.Subject, "exp-1")
	}
}

// A client filtering on a kind the server has never heard of would wait forever on a connection
// that looks perfectly healthy — the silent version of the polling failure. Refusing the
// subscription is the only answer that tells the caller anything.
func TestAnUnknownKindIsRefusedRatherThanSilentlyIgnored(t *testing.T) {
	server := watchServer(t, newFakeEvents())
	response, err := http.Get(server.URL + "/watch?platform_experiment_id=pe-1&kinds=experiment.finished")
	if err != nil {
		t.Fatalf("request: got = %v, want = nil", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status for an unknown kind: got = %v, want = %v", response.StatusCode, http.StatusBadRequest)
	}
}

// A running job knows its own experiment id and nothing else — it is never told which platform
// experiment it belongs to. If subscribing required that id the job could not subscribe at all,
// and hl-watch --experiment would be unusable from inside a workload.
func TestSubscribingByExperimentIDAloneResolvesItsPlatformExperiment(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{"experiment_id": {"exp-1"}})

	source.live <- statusEvent("exp-2", "RUNNING", "agent-a", 100)
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 200)

	if got := client.next(t).Subject; got != "exp-1" {
		t.Errorf("delivered subject: got = %v, want = %v", got, "exp-1")
	}
}

// hl-watch is the whole point of the transport: a model cannot hold a socket across tool calls,
// so a process holds it instead and exits on the condition. This runs the real script against the
// real server — the handshake, the framing and the --until contract in one — because those three
// agreeing separately is not the same as them agreeing with each other, and the failure mode if
// they do not is a wait that always ends at the timeout and never at the answer.
func TestHLWatchExitsOnTheTerminalStateRatherThanOnItsTimeout(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "agents", "coordinator", "experiments", "hl-watch"))
	if err != nil {
		t.Fatalf("locate hl-watch: got = %v, want = nil", err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("no python3 to run hl-watch: %v", err)
	}

	source := newFakeEvents()
	server := watchServer(t, source)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				select {
				case source.live <- statusEvent("exp-1", "COMPLETED", "agent-a", time.Now().UnixMicro()):
				default:
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, script,
		"--url", server.URL, "--experiment", "exp-1",
		"--until", "status in COMPLETED,FAILED,EVICTED", "--timeout", "20")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("hl-watch exit: got = %v, want = nil (exit 0)", err)
	}
	if !strings.Contains(string(output), `"value":"COMPLETED"`) {
		t.Errorf("hl-watch output: got = %v, want = a line carrying COMPLETED", string(output))
	}
}
