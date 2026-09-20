package registry

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// fakeEvents is the change stream reduced to the two rules the real one guarantees: replay
// returns the matching events at or after a cursor, in position order, and the live stream carries
// whatever the database announced. It applies db.EventFilter itself rather than a test-local
// approximation, so a test here fails for the same reason production would.
type fakeEvents struct {
	// mu guards stored: a test adds to it while the server's handler goroutines replay from it.
	mu     sync.Mutex
	stored []db.Event
	live   chan db.Event
	// owner maps a hypothesis id to the agent that registered it; links maps a hypothesis id to the
	// ids it links to. Together they answer Concerns as the graph does.
	owner map[string]string
	links map[string][]string
	// next, when set, is the largest cursor this database could have issued.
	next int64
	// concernsCalls counts the times the agent scope was asked, each one a graph read.
	concernsCalls atomic.Int32
	// listenGate, when set, holds LISTEN back until it is closed: the window in which a client
	// can connect to a hub nothing is feeding.
	listenGate chan struct{}
	// listening is closed once LISTEN is first in effect.
	listening     chan struct{}
	listeningOnce sync.Once
}

func newFakeEvents(stored ...db.Event) *fakeEvents {
	return &fakeEvents{stored: stored, live: make(chan db.Event, 16), listening: make(chan struct{})}
}

func (f *fakeEvents) Replay(_ context.Context, filter db.EventFilter, since int64) ([]db.Event, error) {
	var out []db.Event
	if f.next != 0 && since > f.next {
		return nil, db.ErrUnknownCursor
	}
	if since < 0 {
		since = 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.stored {
		if e.Position >= since && filter.Matches(e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeEvents) store(e db.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored = append(f.stored, e)
}

func (f *fakeEvents) Concerns(_ context.Context, filter db.EventFilter, events []db.Event) ([]db.Event, error) {
	f.concernsCalls.Add(1)
	var kept []db.Event
	for _, e := range events {
		if f.concerns(filter, e) {
			kept = append(kept, e)
		}
	}
	return kept, nil
}

func (f *fakeEvents) concerns(filter db.EventFilter, e db.Event) bool {
	switch {
	case filter.AgentID == "" || !strings.HasPrefix(e.Subject, "hyp-"):
		return true
	case e.Kind == db.EventHypothesisNew:
		for _, to := range f.links[e.Subject] {
			if f.owner[to] == filter.AgentID {
				return true
			}
		}
		return false
	case e.Kind == db.EventHypothesisLink:
		return f.owner[e.Detail] == filter.AgentID
	default:
		return f.owner[e.Subject] == filter.AgentID
	}
}

func (f *fakeEvents) ListenEvents(ctx context.Context, out chan<- db.Event, listening func()) error {
	if f.listenGate != nil {
		select {
		case <-f.listenGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	listening()
	f.listeningOnce.Do(func() { close(f.listening) })
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

func watchServer(t *testing.T, source *fakeEvents) *httptest.Server {
	t.Helper()
	handler := NewWatchHandler(source, &lookupStore{exp: &domain.Experiment{ID: "exp-1", PlatformExperimentID: "pe-1"}}, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	handler.Start(ctx)
	if source.listenGate == nil {
		<-source.listening
	}
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

// sendText sends one masked text frame, which is how a client asks about or re-scopes its own
// subscription. A client must mask everything it sends, so this does.
func (c *watchClient) sendText(t *testing.T, text string) {
	t.Helper()
	payload := []byte(text)
	if len(payload) > 125 {
		t.Fatalf("control frame length: got = %v, want = a short frame this helper can encode", len(payload))
	}
	frame := []byte{0x80 | wsOpText, byte(0x80 | len(payload)), 1, 2, 3, 4}
	for i, b := range payload {
		frame = append(frame, b^frame[2+i%4])
	}
	if _, err := c.conn.Write(frame); err != nil {
		t.Fatalf("write control frame: got = %v, want = nil", err)
	}
}

// next returns the next event the server sent, skipping the pings that keep an idle stream alive.
func (c *watchClient) next(t *testing.T) db.Event {
	t.Helper()
	var e db.Event
	payload := c.nextRaw(t)
	if err := json.Unmarshal(payload, &e); err != nil {
		t.Fatalf("decode event %q: got = %v, want = nil", payload, err)
	}
	return e
}

// nextRaw returns the next text frame's bytes. The stream carries two shapes — an event, and the
// server's answer to a control frame — and a test about the second cannot go through a decoder
// that assumes the first.
func (c *watchClient) nextRaw(t *testing.T) []byte {
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
		return payload
	}
}

func statusEvent(subject, value, agentID string, cursor int64) db.Event {
	return db.Event{Kind: db.EventExperimentStatus, Subject: subject, Value: value,
		PlatformExperimentID: "pe-1", AgentID: agentID, Position: cursor, Cursor: cursor}
}

// A dropped connection must cost a client a delay and never a fact. An agent that reconnects and
// silently skips the transition it was disconnected across would go back to polling full state,
// which is the exact failure this stream exists to end — so replay has to return every event
// from the cursor on, the one at the cursor included, and start the live stream where it stopped.
func TestAReconnectFromTheLargestCursorSeenReplaysEveryEventFromThatCursorOn(t *testing.T) {
	source := newFakeEvents(
		statusEvent("exp-1", "QUEUED", "agent-a", 100),
		statusEvent("exp-1", "SUBMITTED", "agent-a", 200),
		statusEvent("exp-1", "RUNNING", "agent-a", 300),
	)
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"since":                  {"200"},
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

// A client with no cursor at all is not asking to skip history -- it has none to give. Treating
// that the same as an empty subscription used to mean watching a job that was already terminal
// got nothing but pings until the client's own timeout, because a finished job emits no further
// live event. since=0 must answer with the current snapshot, exactly as if the caller's cursor
// predated every row it matches.
func TestNoCursorAtAllGetsTheCurrentSnapshotInsteadOfWaitingOnALiveEventThatWillNeverCome(t *testing.T) {
	source := newFakeEvents(
		statusEvent("exp-1", "COMPLETED", "agent-a", 300),
	)
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
	})

	if got := client.next(t).Value; got != "COMPLETED" {
		t.Fatalf("snapshot on connect with no cursor: got = %v, want = %v", got, "COMPLETED")
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

// One transaction can write several events at the same cursor: a stage boundary cuts every agent
// at one cut_at. Each is news to the agent it names, so only the exact event a replay carried may
// be dropped from the live stream, never its siblings.
func TestEventsSharingACursorAreAllDeliveredAndOnlyTheReplayedOneIsDropped(t *testing.T) {
	cut := func(agentID string) db.Event {
		return db.Event{Kind: db.EventAgentCut, Subject: agentID, Value: "1", PlatformExperimentID: "pe-1", AgentID: agentID, Position: 500, Cursor: 500}
	}
	source := newFakeEvents(cut("agent-a"))
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"kinds":                  {db.EventAgentCut},
		"since":                  {"100"},
	})

	if got := client.next(t).Subject; got != "agent-a" {
		t.Fatalf("replayed cut: got = %v, want = %v", got, "agent-a")
	}
	source.live <- cut("agent-a")
	source.live <- cut("agent-b")
	source.live <- cut("agent-c")
	for _, want := range []string{"agent-b", "agent-c"} {
		if got := client.next(t).Subject; got != want {
			t.Errorf("live cut at the replayed cursor: got = %v, want = %v", got, want)
		}
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
		PlatformExperimentID: "pe-1", Position: 100, Cursor: 100}
	source.live <- db.Event{Kind: db.EventCommentNew, Subject: "hyp-1",
		PlatformExperimentID: "pe-1", Position: 150, Cursor: 150}
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 200)

	got := client.next(t)
	if got.Kind != db.EventExperimentStatus {
		t.Errorf("first delivered kind: got = %v, want = %v", got.Kind, db.EventExperimentStatus)
	}
	if got.Value != "RUNNING" {
		t.Errorf("first delivered value: got = %v, want = %v", got.Value, "RUNNING")
	}
}

func hypEvent(kind, subject, detail, agentID string, position int64) db.Event {
	return db.Event{Kind: kind, Subject: subject, Detail: detail, PlatformExperimentID: "pe-1",
		AgentID: agentID, Position: position, Cursor: position}
}

// An agent scope carries its own outcomes and what other people do to hypotheses it registered: a
// comment, a finding, a link to one, a new hypothesis building on one. A rival's job, activity on
// someone else's hypothesis and the echo of the agent's own writes stay off it. Replay and live go
// through the same rule, so a reconnect cannot widen or narrow what the agent is told.
func TestAnAgentScopeCarriesOthersActivityOnItsOwnHypothesesAndNothingElseFromThePool(t *testing.T) {
	events := []db.Event{
		statusEvent("exp-2", "COMPLETED", "agent-b", 100),
		hypEvent(db.EventCommentNew, "hyp-b", "", "agent-c", 110),
		hypEvent(db.EventCommentNew, "hyp-a", "", "agent-a", 120),
		hypEvent(db.EventCommentNew, "hyp-a", "", "agent-b", 130),
		hypEvent(db.EventFindingNew, "hyp-a", "", "agent-b", 140),
		hypEvent(db.EventHypothesisLink, "hyp-b", "hyp-a", "agent-b", 150),
		hypEvent(db.EventHypothesisLink, "hyp-b", "pe-1", "agent-b", 160),
		hypEvent(db.EventHypothesisNew, "hyp-c", "", "agent-b", 170),
		hypEvent(db.EventHypothesisNew, "hyp-d", "", "agent-b", 180),
		hypEvent(db.EventHypothesisStatus, "hyp-a", "", "agent-a", 190),
		statusEvent("exp-1", "RUNNING", "agent-a", 200),
		statusEvent("exp-1", "COMPLETED", "agent-a", 210),
	}
	want := []int64{130, 140, 150, 170, 210}
	scope := func(source *fakeEvents) *fakeEvents {
		source.owner = map[string]string{"hyp-a": "agent-a", "hyp-b": "agent-b"}
		source.links = map[string][]string{"hyp-c": {"hyp-a"}, "hyp-d": {"hyp-b"}}
		return source
	}
	query := url.Values{"platform_experiment_id": {"pe-1"}, "agent": {"agent-a"}}

	stored := scope(newFakeEvents(events...))
	replayed := dialWatch(t, watchServer(t, stored), query)
	for _, position := range want {
		if got := replayed.next(t).Position; got != position {
			t.Errorf("replayed position: got = %v, want = %v", got, position)
		}
	}
	if got := stored.concernsCalls.Load(); got != 1 {
		t.Errorf("agent scope reads for a replay of %d events: got = %v, want = 1", len(events), got)
	}

	source := scope(newFakeEvents())
	live := dialWatch(t, watchServer(t, source), query)
	for _, e := range events {
		source.live <- e
	}
	for _, position := range want {
		if got := live.next(t).Position; got != position {
			t.Errorf("live position: got = %v, want = %v", got, position)
		}
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
// and hl watch --experiment would be unusable from inside a workload.
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

var (
	hlBinaryOnce sync.Once
	hlBinary     string
	hlBinaryErr  error
)

// runHLWatch runs the real hl binary's watch command against the real server and returns its exit
// code, stdout and stderr: what an agent branching on this tool actually sees.
func runHLWatch(t *testing.T, serverURL string, args ...string) (int, string, string) {
	t.Helper()
	return runHLWatchEnv(t, nil, serverURL, args...)
}

// runHLWatchEnv is runHLWatch with env added to the process environment.
func runHLWatchEnv(t *testing.T, env []string, serverURL string, args ...string) (int, string, string) {
	t.Helper()
	hlBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hl watch-test-")
		if err != nil {
			hlBinaryErr = err
			return
		}
		hlBinary = filepath.Join(dir, "hl")
		out, err := exec.Command("go", "build", "-o", hlBinary, "github.com/scaleresearch/hypothesisloop/cli").CombinedOutput()
		if err != nil {
			hlBinaryErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if hlBinaryErr != nil {
		t.Fatalf("build hl: got = %v, want = nil", hlBinaryErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, hlBinary, append([]string{"watch", "--url", serverURL}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
	default:
		t.Fatalf("run hl watch: got = %v, want = nil or an exit status", runErr)
	}
	return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
}

// hl watch is the whole point of the transport: a model cannot hold a socket across tool calls,
// so a process holds it instead and exits on the condition. This runs the real hl binary against
// the real server — the handshake, the framing and the --until contract in one — because those three
// agreeing separately is not the same as them agreeing with each other, and the failure mode if
// they do not is a wait that always ends at the timeout and never at the answer.
func TestHLWatchExitsOnTheTerminalStateRatherThanOnItsTimeout(t *testing.T) {
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

	code, output, stderr := runHLWatch(t, server.URL, "--experiment", "exp-1",
		"--until", "status in COMPLETED,FAILED,EVICTED", "--timeout", "20")
	if code != 0 {
		t.Fatalf("hl watch exit code: got = %v, want = %v (stderr: %s)", code, 0, stderr)
	}
	if !strings.Contains(output, `"value":"COMPLETED"`) {
		t.Errorf("hl watch output: got = %v, want = a line carrying COMPLETED", output)
	}
}

// A 4xx says the request is wrong, and it will be exactly as wrong on the next attempt. Retrying
// it to the timeout and then exiting 0 is the worst of both: the agent blocks for its whole
// window and then reads success, which is the silent wait hl watch exists to remove. It has to
// fail immediately and say so in the exit code.
func TestHLWatchFailsImmediatelyWhenThePlatformRefusesTheSubscription(t *testing.T) {
	server := watchServer(t, newFakeEvents())

	started := time.Now()
	code, _, stderr := runHLWatch(t, server.URL, "--experiment", "exp-1",
		"--kinds", "not.a.real.kind", "--timeout", "30")

	if code != 2 {
		t.Errorf("hl watch exit code for a refused subscription: got = %v, want = %v (stderr: %s)", code, 2, stderr)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("time spent before giving up: got = %v, want = well under the 30s timeout", elapsed)
	}
}

// The status line alone says a request was refused; only the body says which part of it was
// wrong. An agent that mistypes a kind and is told nothing but "400" has to guess between the
// kind, the id and the agent name — so the refusal's own message has to reach the caller.
func TestHLWatchPrintsThePlatformsReasonForRefusingRatherThanABare400(t *testing.T) {
	server := watchServer(t, newFakeEvents())

	_, _, stderr := runHLWatch(t, server.URL, "--experiment", "exp-1",
		"--kinds", "not.a.real.kind", "--timeout", "30")

	if !strings.Contains(stderr, "not.a.real.kind") {
		t.Errorf("hl watch refusal message: got = %v, want = one naming the offending kind", stderr)
	}
}

// The other half of the same rule: a server that is momentarily unreachable is not a wrong
// request, and giving up on it would throw away the reconnect-and-replay behaviour that makes a
// dropped connection cost a delay instead of a fact.
func TestHLWatchKeepsRetryingAConnectionItCouldNotEstablish(t *testing.T) {
	// A port nothing is listening on: every attempt is refused at the TCP level, which is the
	// transient case.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: got = %v, want = nil", err)
	}
	address := listener.Addr().String()
	listener.Close()

	code, _, stderr := runHLWatch(t, "http://"+address, "--experiment", "exp-1",
		"--until", "status in COMPLETED", "--timeout", "5")

	if code != 124 {
		t.Errorf("hl watch exit code after retrying an unreachable server: got = %v, want = %v (stderr: %s)", code, 124, stderr)
	}
}

// agent.cut is the one new kind that is agent-owned, and that is the whole reason it exists: an
// agent watching for its own stop condition must not be woken by every rival's elimination, and
// must never conclude from one that it was cut itself. Getting this wrong turns a stop condition
// into a false one.
func TestAnAgentCutReachesOnlyTheAgentItNames(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"agent":                  {"agent-a"},
	})

	source.live <- db.Event{Kind: db.EventAgentCut, Subject: "agent-b", Value: "1",
		PlatformExperimentID: "pe-1", AgentID: "agent-b", Position: 100, Cursor: 100}
	source.live <- db.Event{Kind: db.EventAgentCut, Subject: "agent-a", Value: "1",
		PlatformExperimentID: "pe-1", AgentID: "agent-a", Position: 200, Cursor: 200}

	if got := client.next(t).Subject; got != "agent-a" {
		t.Errorf("first delivered cut: got = %v, want = %v", got, "agent-a")
	}
}

// The run-wide kinds are shared, and an agent-scoped subscription is the case that would hide
// them: a ladder advance, a closure and a brief change concern everyone in the run. An agent narrowing to its own jobs and thereby going blind to the run closing would be back
// to polling for the very thing it stopped polling for.
func TestARunWideChangeReachesAnAgentScopedSubscriptionWhoeverWroteIt(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"agent":                  {"agent-a"},
	})

	source.live <- db.Event{Kind: db.EventStageBoundary, Subject: "pe-1", Value: "1", Detail: "advanced",
		PlatformExperimentID: "pe-1", Position: 100, Cursor: 100}
	source.live <- db.Event{Kind: db.EventPlatformExperimentDescription, Subject: "pe-1",
		PlatformExperimentID: "pe-1", Position: 200, Cursor: 200}
	source.live <- db.Event{Kind: db.EventPlatformExperimentStatus, Subject: "pe-1", Value: "closed",
		PlatformExperimentID: "pe-1", Position: 300, Cursor: 300}

	for _, want := range []string{db.EventStageBoundary, db.EventPlatformExperimentDescription, db.EventPlatformExperimentStatus} {
		if got := client.next(t).Kind; got != want {
			t.Errorf("delivered kind: got = %v, want = %v", got, want)
		}
	}
}

// A job subscribes by its own experiment id and gets its own job's events — that scope is what
// makes it safe for a workload to watch at all. Widening it silently to run-wide news would hand a
// job information it has no business acting on, and the platform's rule is that a runtime acts on
// desired state and on nothing else.
func TestAJobScopedSubscriptionDoesNotReceiveTheRunWideKinds(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{"experiment_id": {"exp-1"}})

	source.live <- db.Event{Kind: db.EventPlatformExperimentStatus, Subject: "pe-1", Value: "closed",
		PlatformExperimentID: "pe-1", Position: 100, Cursor: 100}
	source.live <- db.Event{Kind: db.EventAgentCut, Subject: "agent-a", Value: "1",
		PlatformExperimentID: "pe-1", AgentID: "agent-a", Position: 200, Cursor: 200}
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 300)

	if got := client.next(t).Kind; got != db.EventExperimentStatus {
		t.Errorf("first delivered kind: got = %v, want = %v", got, db.EventExperimentStatus)
	}
}

// The vocabulary is published so a subscriber can discover it instead of being told it, and that
// is only worth anything if what is published is what is accepted. A kind advertised but refused
// is the worse of the two failures: the caller does exactly what the documentation said and is
// turned away with a 400 it cannot act on.
func TestEveryAdvertisedKindIsAKindASubscriptionMayName(t *testing.T) {
	for _, kind := range db.EventKinds {
		request := httptest.NewRequest(http.MethodGet,
			"/watch?platform_experiment_id=pe-1&kinds="+url.QueryEscape(kind.Kind), nil)
		filter, _, err := parseWatchQuery(request)
		if err != nil {
			t.Errorf("subscribing to the advertised kind %v: got = %v, want = nil", kind.Kind, err)
			continue
		}
		if !filter.Kinds[kind.Kind] {
			t.Errorf("filter for the advertised kind %v: got = %v, want = it admitted", kind.Kind, filter.Kinds)
		}
	}
}

// The other direction of the same rule. A kind the server accepts but never advertises is folklore
// — an agent can only use it if someone tells it, which is exactly the state publishing the
// vocabulary was meant to end.
func TestEveryKindASubscriptionMayNameIsAlsoAdvertised(t *testing.T) {
	advertised := map[string]bool{}
	for _, kind := range db.EventKinds {
		advertised[kind.Kind] = true
	}
	for kind := range knownKinds {
		if !advertised[kind] {
			t.Errorf("accepted kind %v: got = %v, want = listed by GET /watch/kinds", kind, "unlisted")
		}
	}
}

// The default is the whole point of the feature: an agent that has to name kinds has to know them,
// and an agent that names them wrong waits on a healthy socket forever. A subscription that names
// none must therefore carry every signal the agent's loop would otherwise poll for — its jobs and
// why they are stuck, its allocation, its two stop conditions, the ladder, the brief and the pool.
func TestASubscriptionNamingNoKindsCarriesEverythingTheAgentLoopWouldOtherwisePollFor(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{"platform_experiment_id": {"pe-1"}})

	wanted := []string{
		db.EventExperimentStatus, db.EventExperimentBlocked, db.EventQuotaChanged,
		db.EventAgentCut, db.EventPlatformExperimentStatus, db.EventPlatformExperimentDescription,
		db.EventStageBoundary, db.EventHypothesisNew, db.EventHypothesisStatus,
		db.EventFindingNew, db.EventCommentNew,
	}
	for i, kind := range wanted {
		source.live <- db.Event{Kind: kind, Subject: "s", PlatformExperimentID: "pe-1",
			AgentID: "agent-a", Position: int64(100 + i), Cursor: int64(100 + i)}
	}
	for _, want := range wanted {
		if got := client.next(t).Kind; got != want {
			t.Errorf("delivered kind on the default subscription: got = %v, want = %v", got, want)
		}
	}
}

// The one deliberate exclusion. metric.point fires per sample for every job in the run and carries
// no value, only a pointer, so defaulting it on would spend a run-wide subscriber's whole buffer on
// news it did not ask for and get the connection dropped for lagging — and it is the one kind a
// reconnect cannot replay, which would make the default the only subscription with a hole in it.
func TestTheDefaultSubscriptionLeavesOutTheMetricPointerFirehose(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{"platform_experiment_id": {"pe-1"}})

	source.live <- db.Event{Kind: db.EventMetricPoint, Subject: "exp-1", Value: "loss",
		PlatformExperimentID: "pe-1", AgentID: "agent-a", Position: 100, Cursor: 100}
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 200)

	if got := client.next(t).Kind; got != db.EventExperimentStatus {
		t.Errorf("first delivered kind on the default subscription: got = %v, want = %v", got, db.EventExperimentStatus)
	}
}

// A default that could not be overridden would make the excluded kind unreachable. Naming kinds
// means exactly those kinds, including when what is named is the one the default leaves out —
// otherwise an agent watching one job's progress could never see its samples arrive.
func TestNamingMetricPointExplicitlyStillDeliversItAndNothingElse(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"kinds":                  {"metric.point"},
	})

	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 100)
	source.live <- db.Event{Kind: db.EventMetricPoint, Subject: "exp-1", Value: "loss",
		PlatformExperimentID: "pe-1", AgentID: "agent-a", Position: 200, Cursor: 200}

	if got := client.next(t).Kind; got != db.EventMetricPoint {
		t.Errorf("first delivered kind for an explicit kinds=metric.point: got = %v, want = %v", got, db.EventMetricPoint)
	}
}

// readSubscription reads frames until the server's answer to a control frame arrives, and returns
// it. Events keep flowing while a control frame is in flight, so an answer is something to wait
// for among them rather than the next thing on the wire.
func readSubscription(t *testing.T, client *watchClient) WatchSubscription {
	t.Helper()
	for i := 0; i < 32; i++ {
		var frame watchFrame
		payload := client.nextRaw(t)
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatalf("decode frame %q: got = %v, want = nil", payload, err)
		}
		if frame.Error != "" {
			t.Fatalf("server answer to a control frame: got = %v, want = a subscription", frame.Error)
		}
		if frame.Subscription != nil {
			return *frame.Subscription
		}
	}
	t.Fatalf("server answer to a control frame: got = %v, want = a subscription frame", "none in 32 frames")
	return WatchSubscription{}
}

// An agent that cannot see what it is subscribed to is back to believing what it asked for. The
// answer has to be read from the filter the server is matching against — including the kinds it
// filled in itself when the client named none — or "I am subscribed" means nothing.
func TestAConnectedClientIsToldTheFilterActuallyInForceIncludingTheDefaultItDidNotName(t *testing.T) {
	server := watchServer(t, newFakeEvents())
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"agent":                  {"agent-a"},
	})

	client.sendText(t, "{}")
	got := readSubscription(t, client)

	if got.PlatformExperimentID != "pe-1" || got.AgentID != "agent-a" {
		t.Errorf("reported scope: got = %v/%v, want = pe-1/agent-a", got.PlatformExperimentID, got.AgentID)
	}
	if len(got.Kinds) != len(db.DefaultKinds()) {
		t.Errorf("reported kinds: got = %v, want = the %v default kinds", got.Kinds, len(db.DefaultKinds()))
	}
	for _, kind := range got.Kinds {
		if kind == db.EventMetricPoint {
			t.Errorf("reported kinds: got = %v, want = one without metric.point", got.Kinds)
		}
	}
}

// The two halves of a filter change, in the order that makes them one guarantee. Narrowing must
// stop the kinds it dropped, and widening must deliver the added kind from where this connection
// last stood — a widen that only went live from the moment of the change would leave the client's
// cursor claiming it had seen events it never received, which is a gap no reconnect would ever
// repair.
func TestWideningAFilterReplaysTheAddedKindFromWhereTheConnectionStoodRatherThanGoingLiveOnly(t *testing.T) {
	source := newFakeEvents(
		db.Event{Kind: db.EventHypothesisNew, Subject: "hyp-1", Value: "agent",
			PlatformExperimentID: "pe-1", AgentID: "agent-b", Position: 150, Cursor: 150},
	)
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"kinds":                  {"experiment.status"},
		"since":                  {"100"},
	})

	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 200)
	if got := client.next(t).Value; got != "RUNNING" {
		t.Fatalf("first delivered status: got = %v, want = %v", got, "RUNNING")
	}

	client.sendText(t, `{"kinds": "experiment.status,hypothesis.new"}`)

	// The pool event predates the last status this connection saw, and is delivered anyway:
	// nothing carried that kind before, so nothing has served it.
	if got := client.next(t); got.Kind != db.EventHypothesisNew || got.Cursor != 150 {
		t.Errorf("first frame after widening: got = %v at %v, want = hypothesis.new at 150", got.Kind, got.Cursor)
	}
	if got := readSubscription(t, client); len(got.Kinds) != 2 {
		t.Errorf("reported kinds after widening: got = %v, want = the two named", got.Kinds)
	}
}

// A replay starts at the floor it resumes from, the event that set the floor included. One this
// connection already received live must not be sent again when a narrowed kind is re-added.
func TestReAddingANarrowedKindDoesNotResendTheEventsItAlreadyDeliveredLive(t *testing.T) {
	pool := db.Event{Kind: db.EventHypothesisNew, Subject: "hyp-1", Value: "agent",
		PlatformExperimentID: "pe-1", AgentID: "agent-b", Position: 150, Cursor: 150}
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"kinds":                  {"experiment.status,hypothesis.new"},
		"since":                  {"100"},
	})

	source.store(pool)
	source.live <- pool
	if got := client.next(t); got.Subject != "hyp-1" {
		t.Fatalf("live pool event: got = %v, want = %v", got.Subject, "hyp-1")
	}
	client.sendText(t, `{"kinds": "experiment.status"}`)
	readSubscription(t, client)
	client.sendText(t, `{"kinds": "experiment.status,hypothesis.new"}`)
	readSubscription(t, client)

	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 200)
	if got := client.next(t); got.Kind != db.EventExperimentStatus {
		t.Errorf("next frame after re-adding the kind: got = %v %v, want = the live status, with no repeat of hyp-1", got.Kind, got.Subject)
	}
}

// The other side of the same invariant, and the one that would be silent: an event the pre-change
// filter would have delivered must survive the change. A filter change that quietly swallowed
// whatever was in flight would make re-scoping more dangerous than reconnecting, and an agent would
// be right never to use it.
func TestAFilterChangeDoesNotDropAnEventThePreChangeFilterWouldHaveDelivered(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"kinds":                  {"experiment.status"},
		"since":                  {"100"},
	})

	// Queued under the old filter, and the change is sent without waiting for it to be written:
	// whichever order the server sees them in, the status is owed to this client.
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 200)
	client.sendText(t, `{"kinds": "experiment.status,quota.changed"}`)

	for i := 0; i < 32; i++ {
		var frame watchFrame
		payload := client.nextRaw(t)
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatalf("decode frame %q: got = %v, want = nil", payload, err)
		}
		if frame.Subscription != nil {
			continue
		}
		var e db.Event
		if err := json.Unmarshal(payload, &e); err != nil {
			t.Fatalf("decode event %q: got = %v, want = nil", payload, err)
		}
		if e.Kind == db.EventExperimentStatus && e.Value == "RUNNING" {
			return
		}
	}
	t.Errorf("the status queued under the pre-change filter: got = %v, want = delivered across the change", "never delivered")
}

// Narrowing has to actually narrow, and has to leave the rest alone. A change reported as applied
// but not applied is the worst answer of the three, because the client stops watching for what it
// believes it has already dropped.
func TestNarrowingAFilterStopsTheDroppedKindAndKeepsTheOnesLeft(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{"platform_experiment_id": {"pe-1"}})

	client.sendText(t, `{"kinds": "experiment.status"}`)
	if got := readSubscription(t, client); len(got.Kinds) != 1 || got.Kinds[0] != db.EventExperimentStatus {
		t.Fatalf("reported kinds after narrowing: got = %v, want = [experiment.status]", got.Kinds)
	}

	source.live <- db.Event{Kind: db.EventCommentNew, Subject: "hyp-1", Value: "agent",
		PlatformExperimentID: "pe-1", AgentID: "agent-b", Position: 100, Cursor: 100}
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 200)

	if got := client.next(t).Kind; got != db.EventExperimentStatus {
		t.Errorf("first delivered kind after narrowing: got = %v, want = %v", got, db.EventExperimentStatus)
	}
}

// A kind that does not exist is refused in a control frame for the same reason it is refused in the
// query string: a client whose change was ignored would go on waiting on a filter it does not have.
// The connection survives — the request was wrong, not the socket.
func TestAControlFrameNamingAnUnknownKindIsRefusedAndLeavesTheFilterAlone(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"kinds":                  {"experiment.status"},
	})

	client.sendText(t, `{"kinds": "experiment.finished"}`)
	var frame watchFrame
	payload := client.nextRaw(t)
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("decode frame %q: got = %v, want = nil", payload, err)
	}
	if !strings.Contains(frame.Error, "experiment.finished") {
		t.Errorf("answer to a control frame naming an unknown kind: got = %v, want = one naming it", string(payload))
	}
	source.live <- statusEvent("exp-1", "RUNNING", "agent-a", 200)
	if got := client.next(t).Value; got != "RUNNING" {
		t.Errorf("stream after a refused control frame: got = %v, want = still carrying %v", got, "RUNNING")
	}
}

// Listing subscriptions reads the live connections and nothing else. That is what keeps it from
// being a subscriber registry: an entry exists for exactly as long as its socket does, so a
// watcher that died is not listed — which is the true answer, not a missing one.
func TestListingSubscriptionsReportsTheLiveConnectionsAndForgetsThemWhenTheyClose(t *testing.T) {
	source := newFakeEvents()
	handler := NewWatchHandler(source,
		&lookupStore{exp: &domain.Experiment{ID: "exp-1", PlatformExperimentID: "pe-1"}}, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler.Start(ctx)
	<-source.listening
	server := httptest.NewServer(http.HandlerFunc(handler.ServeHTTP))
	defer server.Close()

	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"agent":                  {"agent-a"},
		"kinds":                  {"experiment.status"},
	})
	client.sendText(t, "{}")
	readSubscription(t, client)

	listed := handler.hub.list("pe-1", "agent-a")
	if len(listed) != 1 {
		t.Fatalf("live subscriptions for agent-a: got = %v, want = %v", len(listed), 1)
	}
	if len(listed[0].Kinds) != 1 || listed[0].Kinds[0] != db.EventExperimentStatus {
		t.Errorf("listed kinds: got = %v, want = [experiment.status]", listed[0].Kinds)
	}
	if got := handler.hub.list("pe-1", "agent-b"); len(got) != 0 {
		t.Errorf("live subscriptions for an agent with none: got = %v, want = %v", len(got), 0)
	}

	client.conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for len(handler.hub.list("pe-1", "agent-a")) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("live subscriptions after the socket closed: got = %v, want = %v", len(handler.hub.list("pe-1", "agent-a")), 0)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The default subscription is the one every agent will use, so the scoping rule has to hold on it
// and not only on the hand-written filters it was first tested with. An agent whose default
// subscription carried a rival's job status or quota would be reading a competitor's timeline, and
// would also be woken by every event in the run rather than by its own.
func TestTheDefaultSubscriptionScopedToAnAgentStillNeverCarriesAnotherAgentsOwnEvents(t *testing.T) {
	source := newFakeEvents()
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{
		"platform_experiment_id": {"pe-1"},
		"agent":                  {"agent-a"},
	})

	source.live <- statusEvent("exp-2", "RUNNING", "agent-b", 100)
	source.live <- db.Event{Kind: db.EventQuotaChanged, Subject: "agent-b", Value: "9",
		PlatformExperimentID: "pe-1", AgentID: "agent-b", Position: 150, Cursor: 150}
	source.live <- db.Event{Kind: db.EventAgentCut, Subject: "agent-b", Value: "1",
		PlatformExperimentID: "pe-1", AgentID: "agent-b", Position: 175, Cursor: 175}
	source.live <- db.Event{Kind: db.EventQuotaChanged, Subject: "agent-a", Value: "4",
		PlatformExperimentID: "pe-1", AgentID: "agent-a", Position: 200, Cursor: 200}

	got := client.next(t)
	if got.Kind != db.EventQuotaChanged || got.Subject != "agent-a" {
		t.Errorf("first delivered event: got = %v for %v, want = quota.changed for agent-a", got.Kind, got.Subject)
	}
}

// The default set is served as part of the vocabulary rather than kept beside it, for the same
// reason the accepted set is: a default an agent is told about but does not get, or gets but is
// never told about, is the failure a second hand-kept list produces.
func TestTheDefaultTheServerAppliesIsTheDefaultGETWatchKindsAdvertises(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/watch?platform_experiment_id=pe-1", nil)
	filter, _, err := parseWatchQuery(request)
	if err != nil {
		t.Fatalf("subscribing without naming kinds: got = %v, want = nil", err)
	}
	for _, kind := range db.EventKinds {
		if filter.Kinds[kind.Kind] != kind.Default {
			t.Errorf("kind %v on a subscription naming none: got = %v, want = %v (as advertised)",
				kind.Kind, filter.Kinds[kind.Kind], kind.Default)
		}
	}
}

// The inspect path end to end, through the real hl binary. hl watch masks what it sends and the
// server unmasks it, and those two agreeing separately is not the same as them agreeing with each
// other — if they do not, an agent asking what it is subscribed to gets a dropped socket instead of
// an answer, which is worse than not being able to ask.
func TestHLWatchPrintsWhatTheServerSaysThisConnectionIsSubscribedTo(t *testing.T) {
	server := watchServer(t, newFakeEvents())

	_, output, stderr := runHLWatch(t, server.URL, "--experiment", "exp-1",
		"--kinds", "experiment.status", "--show-subscription", "--timeout", "5")

	if !strings.Contains(output, `"subscription"`) {
		t.Errorf("hl watch --show-subscription output: got = %v, want = a subscription line (stderr: %s)", output, stderr)
	}
	if !strings.Contains(output, `"experiment.status"`) {
		t.Errorf("hl watch --show-subscription output: got = %v, want = one naming the kind in force", output)
	}
}

func eventLines(output string) []db.Event {
	var events []db.Event
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var e db.Event
		if json.Unmarshal([]byte(line), &e) == nil && e.Kind != "" {
			events = append(events, e)
		}
	}
	return events
}

func positions(events []db.Event) []int64 {
	out := []int64{}
	for _, e := range events {
		out = append(out, e.Position)
	}
	return out
}

func agentEnv(t *testing.T) []string {
	return []string{"AGENT_ID=agent-a", "PLATFORM_EXPERIMENT_ID=pe-1", "HL_STATE_DIR=" + t.TempDir()}
}

// A plain hl watch with no saved cursor returns the current snapshot of the agent's resources. The
// next one resumes from the cursor the first saved: it prints what happened in between and nothing
// the first already printed, though the server replays the events at that cursor again.
func TestHLWatchResumesFromItsSavedCursorWithoutRepeatsOrGaps(t *testing.T) {
	source := newFakeEvents(
		statusEvent("exp-1", "COMPLETED", "agent-a", 100),
		db.Event{Kind: db.EventExperimentStatus, Subject: "exp-2", Value: "FAILED", PlatformExperimentID: "pe-1",
			AgentID: "agent-a", Position: 110, Cursor: 105},
	)
	server := watchServer(t, source)
	env := agentEnv(t)

	code, out, stderr := runHLWatchEnv(t, env, server.URL, "--timeout", "10")
	if got := positions(eventLines(out)); code != 0 || fmt.Sprint(got) != "[100 110]" {
		t.Fatalf("first watch: got = exit %v, positions %v, want = exit 0, [100 110] (stderr: %s)", code, got, stderr)
	}

	source.store(statusEvent("exp-3", "EVICTED", "agent-a", 120))
	code, out, stderr = runHLWatchEnv(t, env, server.URL, "--timeout", "10")
	if got := positions(eventLines(out)); code != 0 || fmt.Sprint(got) != "[120]" {
		t.Errorf("second watch: got = exit %v, positions %v, want = exit 0, [120] (stderr: %s)", code, got, stderr)
	}
}

// A saved cursor this database never issued (it was reset, say) falls back to the snapshot once
// instead of failing the watch.
func TestHLWatchFallsBackToTheSnapshotWhenTheSavedCursorIsUnknown(t *testing.T) {
	source := newFakeEvents(statusEvent("exp-1", "COMPLETED", "agent-a", 100))
	source.next = 200
	server := watchServer(t, source)
	env := agentEnv(t)
	dir := strings.TrimPrefix(env[2], "HL_STATE_DIR=")
	if err := os.WriteFile(filepath.Join(dir, "watch-pe-1-agent-a.json"), []byte(`{"cursor":5000,"seen":[]}`), 0o644); err != nil {
		t.Fatalf("write cursor file: got = %v, want = nil", err)
	}

	code, out, stderr := runHLWatchEnv(t, env, server.URL, "--timeout", "10")
	if got := positions(eventLines(out)); code != 0 || fmt.Sprint(got) != "[100]" {
		t.Errorf("watch from an unknown cursor: got = exit %v, positions %v, want = exit 0, [100] (stderr: %s)", code, got, stderr)
	}
}

// An explicit --since is the caller's statement of where to start, and wins over the saved cursor.
func TestHLWatchExplicitSinceWinsOverTheSavedCursor(t *testing.T) {
	source := newFakeEvents(statusEvent("exp-1", "COMPLETED", "agent-a", 100), statusEvent("exp-2", "COMPLETED", "agent-a", 200))
	server := watchServer(t, source)
	env := agentEnv(t)
	if code, _, stderr := runHLWatchEnv(t, env, server.URL, "--timeout", "10"); code != 0 {
		t.Fatalf("first watch: got = exit %v, want = 0 (stderr: %s)", code, stderr)
	}
	code, out, stderr := runHLWatchEnv(t, env, server.URL, "--since", "150", "--timeout", "10")
	if got := positions(eventLines(out)); code != 0 || fmt.Sprint(got) != "[200]" {
		t.Errorf("watch with --since 150: got = exit %v, positions %v, want = exit 0, [200] (stderr: %s)", code, got, stderr)
	}
}

type countingConn struct {
	net.Conn
	writes int
}

func (c *countingConn) Write(p []byte) (int, error) {
	c.writes++
	return len(p), nil
}

func (c *countingConn) SetWriteDeadline(time.Time) error { return nil }

// A connection that lives for a whole run must not remember every event it ever sent: once a kind's
// floor passes an event, a repeat of it is recognised by its position.
func TestAConnectionForgetsTheEventsItsFloorHasPassedAndStillDropsTheirRepeats(t *testing.T) {
	wire := &countingConn{}
	conn := &wsConn{conn: wire, rw: bufio.NewReadWriter(bufio.NewReader(strings.NewReader("")), bufio.NewWriter(wire))}
	delivered := &served{ctx: context.Background(), source: newFakeEvents(), logger: zap.NewNop(),
		floor: map[string]int64{}, sent: map[db.Event]bool{}}
	filter := db.EventFilter{PlatformExperimentID: "pe-1"}

	for cursor := int64(1); cursor <= 500; cursor++ {
		if err := delivered.send(conn, filter, statusEvent("job-1", "RUNNING", "agent-1", cursor)); err != nil {
			t.Fatal(err)
		}
	}
	if len(delivered.sent) != 1 {
		t.Errorf("identities remembered after 500 events = %d, want only the one at the floor", len(delivered.sent))
	}
	written := wire.writes
	for _, cursor := range []int64{3, 500} {
		if err := delivered.send(conn, filter, statusEvent("job-1", "RUNNING", "agent-1", cursor)); err != nil {
			t.Fatal(err)
		}
	}
	if wire.writes != written {
		t.Errorf("a repeat of an event already sent was written again")
	}
	if err := delivered.send(conn, filter, statusEvent("job-2", "RUNNING", "agent-1", 500)); err != nil {
		t.Fatal(err)
	}
	if wire.writes == written {
		t.Errorf("another event of the transaction at the floor was dropped")
	}
}

// A client that connects while LISTEN is down replays and then waits on a hub nothing feeds.
// Whatever commits before LISTEN is back is in neither its replay nor its stream, and later
// cursors pass it, so the connection has to end once LISTEN is back: the reconnect replays.
func TestAClientConnectedWhileNothingWasListeningIsDroppedOnceListenIsBack(t *testing.T) {
	source := newFakeEvents(statusEvent("exp-1", "RUNNING", "agent-a", 100))
	source.listenGate = make(chan struct{})
	server := watchServer(t, source)
	client := dialWatch(t, server, url.Values{"platform_experiment_id": {"pe-1"}})
	if got := client.next(t).Value; got != "RUNNING" {
		t.Fatalf("replay on connect: got = %v, want = %v", got, "RUNNING")
	}

	close(source.listenGate)

	if err := client.conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: got = %v, want = nil", err)
	}
	_, err := io.Copy(io.Discard, client.r)
	if err != nil {
		t.Fatalf("connection after LISTEN came back: got = %v, want = closed by the server", err)
	}
}
