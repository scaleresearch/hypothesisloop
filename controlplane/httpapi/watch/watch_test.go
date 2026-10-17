package watch

import (
	"bufio"
	"context"
	"encoding/base64"
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

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// fakeChanges is the changes table reduced to the rules the store guarantees: ids rise with every
// append, After returns the changes of one platform experiment after an id, refuses an id never
// issued or no longer held, and every append wakes the one listener.
type fakeChanges struct {
	mu     sync.Mutex
	stored []domain.Event
	head   int64
	// oldest is the oldest id still held, 0 while nothing was pruned.
	oldest int64
	woke   func()
	// listenGate, when set, holds LISTEN back until it is closed.
	listenGate    chan struct{}
	listening     chan struct{}
	listeningOnce sync.Once
}

func newFakeChanges(stored ...domain.Event) *fakeChanges {
	f := &fakeChanges{listening: make(chan struct{})}
	for _, e := range stored {
		f.append(e)
	}
	return f
}

// append adds e as the next change and wakes the listener, if one listens.
func (f *fakeChanges) append(e domain.Event) int64 {
	f.mu.Lock()
	f.head++
	e.ID = f.head
	f.stored = append(f.stored, e)
	woke := f.woke
	f.mu.Unlock()
	if woke != nil {
		woke()
	}
	return e.ID
}

func (f *fakeChanges) After(_ context.Context, platformExperimentID string, after int64, limit int) ([]domain.Event, int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if after > f.head {
		return nil, 0, false, db.ErrChangeUnknown
	}
	if after > 0 && after < f.oldest-1 {
		return nil, 0, false, db.ErrChangesPruned
	}
	var out []domain.Event
	for _, e := range f.stored {
		if e.ID > after && e.PlatformExperimentID == platformExperimentID {
			out = append(out, e)
			if len(out) == limit {
				return out, e.ID, false, nil
			}
		}
	}
	return out, f.head, true, nil
}

// ProjectAfter is the store's to scope (db.Store.ProjectAfter): the fake places no platform
// experiment in a project.
func (f *fakeChanges) ProjectAfter(context.Context, domain.TenantScope, bool, int64, int) ([]domain.Event, int64, bool, error) {
	return nil, 0, false, errors.New("fakeChanges: no project scope")
}

func (f *fakeChanges) Head(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, nil
}

func (f *fakeChanges) Listen(ctx context.Context, woke func()) error {
	if f.listenGate != nil {
		select {
		case <-f.listenGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	f.woke = woke
	f.mu.Unlock()
	woke()
	f.listeningOnce.Do(func() { close(f.listening) })
	<-ctx.Done()
	return ctx.Err()
}

// fakeHypotheses answers ConcernsAgent as the graph does from who registered each hypothesis and
// what each links to.
type fakeHypotheses struct {
	owner map[string]string
	links map[string][]string
	// calls counts the times it was asked, each one a graph read.
	calls atomic.Int32
}

func (f *fakeHypotheses) ConcernsAgent(_ context.Context, agentID string, events []domain.Event) ([]bool, error) {
	f.calls.Add(1)
	out := make([]bool, len(events))
	for i, e := range events {
		switch e.Kind {
		case domain.EventHypothesisNew:
			for _, to := range f.links[e.Subject] {
				out[i] = out[i] || f.owner[to] == agentID
			}
		case domain.EventHypothesisLink:
			out[i] = f.owner[e.Detail] == agentID
		default:
			out[i] = f.owner[e.Subject] == agentID
		}
	}
	return out, nil
}

// lookupStore places every job in exp's platform experiment.
type lookupStore struct {
	exp *domain.Experiment
}

func (s *lookupStore) GetExperiment(context.Context, string) (*domain.Experiment, error) {
	return s.exp, nil
}

func newTestHandler(changes *fakeChanges, hypotheses *fakeHypotheses, placement authtest.Placement) *Handler {
	return NewHandler(changes, hypotheses, &lookupStore{exp: &domain.Experiment{ID: "exp-1", PlatformExperimentID: "pe-1"}}, auth.NewAccess(placement), zap.NewNop())
}

// serve starts handler's LISTEN and serves it behind tokens until the test ends.
func serve(t *testing.T, changes *fakeChanges, handler *Handler, tokens auth.Tokens) *httptest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	handler.Start(ctx)
	if changes.listenGate == nil {
		<-changes.listening
	}
	server := httptest.NewServer(gated(tokens, handler))
	t.Cleanup(func() {
		server.Close()
		cancel()
	})
	return server
}

func watchServer(t *testing.T, changes *fakeChanges) *httptest.Server {
	t.Helper()
	return watchServerWith(t, changes, &fakeHypotheses{})
}

func watchServerWith(t *testing.T, changes *fakeChanges, hypotheses *fakeHypotheses) *httptest.Server {
	t.Helper()
	return serve(t, changes, newTestHandler(changes, hypotheses, authtest.Placement{}), authtest.Staff)
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
		"Sec-WebSocket-Protocol: " + Subprotocol + ", test\r\n" +
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
	accept := ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read handshake headers: got = %v, want = nil", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Sec-WebSocket-Accept") {
			accept = strings.TrimSpace(value)
		}
	}
	// The key above and this answer are the example of RFC 6455 section 1.3. A proxy in front of
	// the platform refuses an upgrade answered with anything else.
	if accept != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("Sec-WebSocket-Accept: got = %q, want = the answer RFC 6455 gives to the key sent", accept)
	}
	return &watchClient{conn: conn, r: r}
}

func (c *watchClient) frame(t *testing.T) platformapi.Frame {
	t.Helper()
	var f platformapi.Frame
	payload := c.nextRaw(t)
	if err := json.Unmarshal(payload, &f); err != nil {
		t.Fatalf("decode frame %q: got = %v, want = nil", payload, err)
	}
	return f
}

// next returns the next change the server sent, skipping the subscription and progress frames.
func (c *watchClient) next(t *testing.T) domain.Event {
	t.Helper()
	for {
		if f := c.frame(t); f.Event != nil {
			return *f.Event
		}
	}
}

// progress returns the next progress frame, failing on a change before it.
func (c *watchClient) progress(t *testing.T) (int64, bool) {
	t.Helper()
	for {
		f := c.frame(t)
		if f.Event != nil {
			t.Fatalf("frame: got = change %+v, want = a progress frame", *f.Event)
		}
		if f.Progress != nil {
			return f.Next, f.CaughtUp
		}
	}
}

// readSubscription returns the first frame of a stream, the subscription in force.
func readSubscription(t *testing.T, client *watchClient) platformapi.Subscription {
	t.Helper()
	f := client.frame(t)
	if f.Subscription == nil {
		t.Fatalf("first frame: got = %+v, want = the subscription", f)
	}
	return *f.Subscription
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

func statusEvent(subject, value, agentID string) domain.Event {
	return domain.Event{Change: domain.Change{Kind: domain.EventExperimentStatus, Subject: subject, Value: value, PlatformExperimentID: "pe-1", AgentID: agentID}}
}

func ids(events []domain.Event) []int64 {
	out := []int64{}
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

// A stream reads on from after: every change after it, then what is appended live, on one path.
// A dropped connection costs a client a delay and never a fact.
func TestAStreamReadsEveryChangeAfterItsAfterAndThenWhatIsAppended(t *testing.T) {
	changes := newFakeChanges(statusEvent("exp-1", "QUEUED", "agent-a"), statusEvent("exp-1", "SUBMITTED", "agent-a"), statusEvent("exp-1", "RUNNING", "agent-a"))
	client := dialWatch(t, watchServer(t, changes), url.Values{"platform_experiment_id": {"pe-1"}, "after": {"1"}})

	if got := readSubscription(t, client).After; got != 1 {
		t.Errorf("subscription after: got = %d, want = 1", got)
	}
	for _, want := range []string{"SUBMITTED", "RUNNING"} {
		if got := client.next(t).Value; got != want {
			t.Errorf("status read: got = %v, want = %v", got, want)
		}
	}
	if next, caughtUp := client.progress(t); next != 3 || !caughtUp {
		t.Errorf("progress: got = %d, %v, want = 3, caught up", next, caughtUp)
	}
	changes.append(statusEvent("exp-1", "COMPLETED", "agent-a"))
	if got := client.next(t); got.Value != "COMPLETED" || got.ID != 4 {
		t.Errorf("live change: got = %+v, want = COMPLETED with id 4", got)
	}
}

// A client with no after reads every change still held: watching a job that already ended answers
// with its end instead of waiting on a change that will never come.
func TestNoAfterReadsEveryChangeStillHeld(t *testing.T) {
	client := dialWatch(t, watchServer(t, newFakeChanges(statusEvent("exp-1", "COMPLETED", "agent-a"))), url.Values{"platform_experiment_id": {"pe-1"}})
	if got := client.next(t).Value; got != "COMPLETED" {
		t.Fatalf("change on connect with no after: got = %v, want = COMPLETED", got)
	}
}

// after=head reads only what is appended from then on.
func TestAfterHeadReadsOnlyWhatComesNext(t *testing.T) {
	changes := newFakeChanges(statusEvent("exp-1", "RUNNING", "agent-a"))
	client := dialWatch(t, watchServer(t, changes), url.Values{"platform_experiment_id": {"pe-1"}, "after": {"head"}})
	if got := readSubscription(t, client).After; got != 1 {
		t.Errorf("subscription after: got = %d, want = the head, 1", got)
	}
	if next, caughtUp := client.progress(t); next != 1 || !caughtUp {
		t.Errorf("progress: got = %d, %v, want = 1, caught up", next, caughtUp)
	}
	changes.append(statusEvent("exp-1", "COMPLETED", "agent-a"))
	if got := client.next(t).Value; got != "COMPLETED" {
		t.Errorf("change after head: got = %v, want = COMPLETED", got)
	}
}

// A progress frame moves past changes the subscription leaves out, and past other platform
// experiments' changes, so a client that saves it never reads them again.
func TestProgressMovesPastChangesTheSubscriptionLeavesOut(t *testing.T) {
	changes := newFakeChanges(
		statusEvent("exp-1", "RUNNING", "agent-a"),
		domain.Event{Change: domain.Change{Kind: domain.EventCommentNew, Subject: "hyp-1", PlatformExperimentID: "pe-1"}},
		domain.Event{Change: domain.Change{Kind: domain.EventExperimentStatus, Subject: "exp-9", Value: "RUNNING", PlatformExperimentID: "pe-other"}},
	)
	client := dialWatch(t, watchServer(t, changes), url.Values{"platform_experiment_id": {"pe-1"}, "kinds": {"experiment.status"}})
	if got := client.next(t).ID; got != 1 {
		t.Errorf("change: got = id %d, want = 1", got)
	}
	if next, caughtUp := client.progress(t); next != 3 || !caughtUp {
		t.Errorf("progress: got = %d, %v, want = 3, caught up", next, caughtUp)
	}
}

// A backlog larger than one read arrives in pages, each ending with progress that is not caught up
// until the last.
func TestABacklogArrivesInPages(t *testing.T) {
	changes := newFakeChanges()
	for i := 0; i < pageSize+1; i++ {
		changes.append(statusEvent("exp-1", "RUNNING", "agent-a"))
	}
	client := dialWatch(t, watchServer(t, changes), url.Values{"platform_experiment_id": {"pe-1"}})
	for i := 0; i < pageSize; i++ {
		client.next(t)
	}
	if next, caughtUp := client.progress(t); next != pageSize || caughtUp {
		t.Errorf("first progress: got = %d, %v, want = %d, not caught up", next, caughtUp, pageSize)
	}
	if got := client.next(t).ID; got != pageSize+1 {
		t.Errorf("last change: got = id %d, want = %d", got, pageSize+1)
	}
	if next, caughtUp := client.progress(t); next != pageSize+1 || !caughtUp {
		t.Errorf("last progress: got = %d, %v, want = %d, caught up", next, caughtUp, pageSize+1)
	}
}

// An after this platform never issued, and one older than what is held, are refused with 410 and
// a message that says what to do.
func TestAnAfterNotIssuedOrNoLongerHeldIsGone(t *testing.T) {
	changes := newFakeChanges(statusEvent("exp-1", "RUNNING", "agent-a"), statusEvent("exp-1", "RUNNING", "agent-a"), statusEvent("exp-1", "RUNNING", "agent-a"))
	changes.oldest = 3
	server := watchServer(t, changes)
	for after, want := range map[string]string{"9": "not a change id this platform issued", "1": "no longer held; re-read state and watch from head"} {
		for _, path := range []string{"/watch", "/changes"} {
			request, _ := http.NewRequest(http.MethodGet, server.URL+path+"?platform_experiment_id=pe-1&after="+after, nil)
			request.Header.Set("Sec-WebSocket-Protocol", Subprotocol+", test")
			request.Header.Set("Authorization", "Bearer test")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusGone || !strings.Contains(string(body), want) {
				t.Errorf("%s after=%s: got = %d %s, want = 410 saying %q", path, after, response.StatusCode, body, want)
			}
		}
	}
}

// since was the parameter of the stream before changes had ids. A client still sending it is told
// what replaced it instead of being served from the start.
func TestSinceIsRefusedNamingAfter(t *testing.T) {
	_, _, err := parseWatchQuery(httptest.NewRequest(http.MethodGet, "/watch?platform_experiment_id=pe-1&since=5", nil))
	if err == nil || !strings.Contains(err.Error(), "after") {
		t.Errorf("since: got = %v, want = a refusal naming after", err)
	}
}

// GET /changes answers one read of what GET /watch would stream.
func TestChangesAnswersOneRead(t *testing.T) {
	changes := newFakeChanges(statusEvent("exp-1", "QUEUED", "agent-a"), statusEvent("exp-1", "RUNNING", "agent-a"), statusEvent("exp-1", "COMPLETED", "agent-a"))
	server := watchServer(t, changes)
	for query, want := range map[string]Page{
		"after=1&limit=1": {Changes: []domain.Event{{ID: 2}}, Next: 2},
		"after=1":         {Changes: []domain.Event{{ID: 2}, {ID: 3}}, Next: 3, CaughtUp: true},
		"after=head":      {Changes: []domain.Event{}, Next: 3, CaughtUp: true},
	} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/changes?platform_experiment_id=pe-1&"+query, nil)
		request.Header.Set("Authorization", "Bearer test")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		var got Page
		err = json.NewDecoder(response.Body).Decode(&got)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s: got = %d (%v), want = 200", query, response.StatusCode, err)
		}
		if fmt.Sprint(ids(got.Changes)) != fmt.Sprint(ids(want.Changes)) || got.Next != want.Next || got.CaughtUp != want.CaughtUp {
			t.Errorf("%s: got = ids %v next %d caught up %v, want = ids %v next %d caught up %v", query,
				ids(got.Changes), got.Next, got.CaughtUp, ids(want.Changes), want.Next, want.CaughtUp)
		}
	}
}

// A filter that named a kind the client did not ask for would make --until fire on a change the
// caller never meant to wait for.
func TestASubscriptionNamingKindsReceivesOnlyThoseKinds(t *testing.T) {
	changes := newFakeChanges()
	client := dialWatch(t, watchServer(t, changes), url.Values{"platform_experiment_id": {"pe-1"}, "kinds": {"experiment.status"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventHypothesisNew, Subject: "hyp-1", PlatformExperimentID: "pe-1"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventCommentNew, Subject: "hyp-1", PlatformExperimentID: "pe-1"}})
	changes.append(statusEvent("exp-1", "RUNNING", "agent-a"))
	if got := client.next(t); got.Kind != domain.EventExperimentStatus || got.Value != "RUNNING" {
		t.Errorf("first delivered: got = %v %v, want = experiment.status RUNNING", got.Kind, got.Value)
	}
}

func hypEvent(kind, subject, detail, agentID string) domain.Event {
	return domain.Event{Change: domain.Change{Kind: kind, Subject: subject, Detail: detail, PlatformExperimentID: "pe-1", AgentID: agentID}}
}

// An agent scope carries its own outcomes and what other people do to hypotheses it registered: a
// comment, a finding, a link to one, a new hypothesis building on one. A rival's job, activity on
// someone else's hypothesis and the echo of the agent's own writes stay off it. The graph is read
// once per page.
func TestAnAgentScopeCarriesOthersActivityOnItsOwnHypothesesAndNothingElseFromThePool(t *testing.T) {
	changes := newFakeChanges(
		statusEvent("exp-2", "COMPLETED", "agent-b"),
		hypEvent(domain.EventCommentNew, "hyp-b", "", "agent-c"),
		hypEvent(domain.EventCommentNew, "hyp-a", "", "agent-a"),
		hypEvent(domain.EventCommentNew, "hyp-a", "", "agent-b"),
		hypEvent(domain.EventFindingNew, "hyp-a", "", "agent-b"),
		hypEvent(domain.EventHypothesisLink, "hyp-b", "hyp-a", "agent-b"),
		hypEvent(domain.EventHypothesisLink, "hyp-b", "pe-1", "agent-b"),
		hypEvent(domain.EventHypothesisNew, "hyp-c", "", "agent-b"),
		hypEvent(domain.EventHypothesisNew, "hyp-d", "", "agent-b"),
		hypEvent(domain.EventHypothesisStatus, "hyp-a", "", "agent-a"),
		statusEvent("exp-1", "RUNNING", "agent-a"),
		statusEvent("exp-1", "COMPLETED", "agent-a"),
	)
	hypotheses := &fakeHypotheses{owner: map[string]string{"hyp-a": "agent-a", "hyp-b": "agent-b"}, links: map[string][]string{"hyp-c": {"hyp-a"}, "hyp-d": {"hyp-b"}}}
	client := dialWatch(t, watchServerWith(t, changes, hypotheses), url.Values{"platform_experiment_id": {"pe-1"}, "agent_id": {"agent-a"}})
	var got []domain.Event
	for range 5 {
		got = append(got, client.next(t))
	}
	if fmt.Sprint(ids(got)) != "[4 5 6 8 12]" {
		t.Errorf("ids: got = %v, want = [4 5 6 8 12]", ids(got))
	}
	if got := hypotheses.calls.Load(); got != 1 {
		t.Errorf("graph reads for one page: got = %v, want = 1", got)
	}
}

// A client filtering on a kind the server has never heard of would wait forever on a connection
// that looks healthy, so the subscription is refused.
func TestAnUnknownKindIsRefusedRatherThanSilentlyIgnored(t *testing.T) {
	server := watchServer(t, newFakeChanges())
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/watch?platform_experiment_id=pe-1&kinds=experiment.finished", nil)
	request.Header.Set("Sec-WebSocket-Protocol", Subprotocol+", test")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: got = %v, want = nil", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("status for an unknown kind: got = %v, want = %v", response.StatusCode, http.StatusBadRequest)
	}
}

// A running job knows its own experiment id and nothing else, so subscribing by it alone has to
// work.
func TestSubscribingByExperimentIDAloneResolvesItsPlatformExperiment(t *testing.T) {
	changes := newFakeChanges()
	client := dialWatch(t, watchServer(t, changes), url.Values{"experiment_id": {"exp-1"}})
	changes.append(statusEvent("exp-2", "RUNNING", "agent-a"))
	changes.append(statusEvent("exp-1", "RUNNING", "agent-a"))
	if got := client.next(t).Subject; got != "exp-1" {
		t.Errorf("delivered subject: got = %v, want = exp-1", got)
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
	cmd := exec.CommandContext(ctx, hlBinary, append([]string{"watch"}, args...)...)
	cmd.Env = append(append(os.Environ(), "HL_TOKEN=test", "API_URL="+serverURL), env...)
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

// hl watch run against the real server exits on the condition, not on its timeout.
func TestHLWatchExitsOnTheTerminalStateRatherThanOnItsTimeout(t *testing.T) {
	changes := newFakeChanges()
	server := watchServer(t, changes)
	go func() {
		time.Sleep(500 * time.Millisecond)
		changes.append(statusEvent("exp-1", "COMPLETED", "agent-a"))
	}()
	code, output, stderr := runHLWatch(t, server.URL, "--experiment", "exp-1", "--until", "status in COMPLETED,FAILED,EVICTED", "--timeout", "20")
	if code != 0 {
		t.Fatalf("hl watch exit code: got = %v, want = 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(output, `"value":"COMPLETED"`) {
		t.Errorf("hl watch output: got = %v, want = a line carrying COMPLETED", output)
	}
}

// A 4xx says the request is wrong and will stay wrong: hl watch fails at once with exit 2 and the
// platform's reason.
func TestHLWatchFailsImmediatelyWithThePlatformsReasonWhenRefused(t *testing.T) {
	server := watchServer(t, newFakeChanges())
	started := time.Now()
	code, _, stderr := runHLWatch(t, server.URL, "--experiment", "exp-1", "--kinds", "not.a.real.kind", "--timeout", "30")
	if code != 2 {
		t.Errorf("exit code: got = %v, want = 2 (stderr: %s)", code, stderr)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("time spent before giving up: got = %v, want = well under the 30s timeout", elapsed)
	}
	if !strings.Contains(stderr, "not.a.real.kind") {
		t.Errorf("refusal message: got = %v, want = one naming the offending kind", stderr)
	}
}

// A server that is momentarily unreachable is not a wrong request: hl watch keeps trying.
func TestHLWatchKeepsRetryingAConnectionItCouldNotEstablish(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: got = %v, want = nil", err)
	}
	address := listener.Addr().String()
	listener.Close()
	code, _, stderr := runHLWatch(t, "http://"+address, "--experiment", "exp-1", "--until", "status in COMPLETED", "--timeout", "5")
	if code != 124 {
		t.Errorf("exit code after retrying an unreachable server: got = %v, want = 124 (stderr: %s)", code, stderr)
	}
}

// An agent watching for its own stop condition must not be woken by a rival's cut.
func TestAnAgentCutReachesOnlyTheAgentItNames(t *testing.T) {
	changes := newFakeChanges()
	client := dialWatch(t, watchServer(t, changes), url.Values{"platform_experiment_id": {"pe-1"}, "agent_id": {"agent-a"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventAgentCut, Subject: "agent-b", Value: "1", PlatformExperimentID: "pe-1", AgentID: "agent-b"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventAgentCut, Subject: "agent-a", Value: "1", PlatformExperimentID: "pe-1", AgentID: "agent-a"}})
	if got := client.next(t).Subject; got != "agent-a" {
		t.Errorf("first delivered cut: got = %v, want = agent-a", got)
	}
}

// A ladder advance, a closure and a brief change concern everyone in the run, so an agent scope
// carries them whoever wrote them.
func TestARunWideChangeReachesAnAgentScopedSubscriptionWhoeverWroteIt(t *testing.T) {
	changes := newFakeChanges()
	client := dialWatch(t, watchServer(t, changes), url.Values{"platform_experiment_id": {"pe-1"}, "agent_id": {"agent-a"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventStageBoundary, Subject: "pe-1", Value: "1", Detail: "advanced", PlatformExperimentID: "pe-1"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventPlatformExperimentDescription, Subject: "pe-1", PlatformExperimentID: "pe-1"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventPlatformExperimentStatus, Subject: "pe-1", Value: "closed", PlatformExperimentID: "pe-1"}})
	for _, want := range []string{domain.EventStageBoundary, domain.EventPlatformExperimentDescription, domain.EventPlatformExperimentStatus} {
		if got := client.next(t).Kind; got != want {
			t.Errorf("delivered kind: got = %v, want = %v", got, want)
		}
	}
}

// A job subscribes by its own experiment id and gets its own job's changes, not run-wide news.
func TestAJobScopedSubscriptionDoesNotReceiveTheRunWideKinds(t *testing.T) {
	changes := newFakeChanges()
	client := dialWatch(t, watchServer(t, changes), url.Values{"experiment_id": {"exp-1"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventPlatformExperimentStatus, Subject: "pe-1", Value: "closed", PlatformExperimentID: "pe-1"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventAgentCut, Subject: "agent-a", Value: "1", PlatformExperimentID: "pe-1", AgentID: "agent-a"}})
	changes.append(statusEvent("exp-1", "RUNNING", "agent-a"))
	if got := client.next(t).Kind; got != domain.EventExperimentStatus {
		t.Errorf("first delivered kind: got = %v, want = experiment.status", got)
	}
}

// What GET /watch/kinds advertises is what a subscription may name, and the other way round.
func TestTheAdvertisedKindsAreTheKindsASubscriptionMayName(t *testing.T) {
	advertised := map[string]bool{}
	for _, kind := range domain.EventKinds {
		advertised[kind.Kind] = true
		filter, _, err := parseWatchQuery(httptest.NewRequest(http.MethodGet, "/watch?platform_experiment_id=pe-1&kinds="+url.QueryEscape(kind.Kind), nil))
		if err != nil || !filter.Kinds[kind.Kind] {
			t.Errorf("subscribing to the advertised kind %v: got = %v (%v), want = it admitted", kind.Kind, filter.Kinds, err)
		}
	}
	for kind := range knownKinds {
		if !advertised[kind] {
			t.Errorf("accepted kind %v: got = unlisted, want = listed by GET /watch/kinds", kind)
		}
	}
}

// A subscription that names no kinds carries every kind.
func TestASubscriptionNamingNoKindsCarriesEveryKind(t *testing.T) {
	filter, _, err := parseWatchQuery(httptest.NewRequest(http.MethodGet, "/watch?platform_experiment_id=pe-1", nil))
	if err != nil {
		t.Fatalf("subscribing without naming kinds: got = %v, want = nil", err)
	}
	for _, kind := range domain.EventKinds {
		if !filter.Kinds[kind.Kind] {
			t.Errorf("kind %v on a subscription naming none: got = left out, want = carried", kind.Kind)
		}
	}
}

// The first frame says what the server matches against, including the kinds it filled in itself
// and the agent it took from the token.
func TestTheFirstFrameIsTheFilterInForce(t *testing.T) {
	client := dialWatch(t, watchServer(t, newFakeChanges()), url.Values{"platform_experiment_id": {"pe-1"}, "agent_id": {"agent-a"}})
	got := readSubscription(t, client)
	if got.PlatformExperimentID != "pe-1" || got.AgentID != "agent-a" || len(got.Kinds) != len(domain.EventKinds) {
		t.Errorf("subscription: got = %+v, want = pe-1/agent-a with every kind", got)
	}
}

// An agent scope never carries another agent's own changes.
func TestAnAgentScopeNeverCarriesAnotherAgentsOwnChanges(t *testing.T) {
	changes := newFakeChanges()
	client := dialWatch(t, watchServer(t, changes), url.Values{"platform_experiment_id": {"pe-1"}, "agent_id": {"agent-a"}})
	changes.append(statusEvent("exp-2", "RUNNING", "agent-b"))
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventQuotaChanged, Subject: "agent-b", Value: "9", PlatformExperimentID: "pe-1", AgentID: "agent-b"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventAgentCut, Subject: "agent-b", Value: "1", PlatformExperimentID: "pe-1", AgentID: "agent-b"}})
	changes.append(domain.Event{Change: domain.Change{Kind: domain.EventQuotaChanged, Subject: "agent-a", Value: "4", PlatformExperimentID: "pe-1", AgentID: "agent-a"}})
	if got := client.next(t); got.Kind != domain.EventQuotaChanged || got.Subject != "agent-a" {
		t.Errorf("first delivered: got = %v for %v, want = quota.changed for agent-a", got.Kind, got.Subject)
	}
}

// hl watch --show-subscription prints the first frame.
func TestHLWatchPrintsWhatTheServerSaysThisConnectionIsSubscribedTo(t *testing.T) {
	server := watchServer(t, newFakeChanges())
	_, output, stderr := runHLWatch(t, server.URL, "--experiment", "exp-1", "--kinds", "experiment.status", "--show-subscription", "--no-wait", "--timeout", "5")
	if !strings.Contains(output, `"subscription"`) || !strings.Contains(output, `"experiment.status"`) {
		t.Errorf("output: got = %v, want = a subscription line naming the kind in force (stderr: %s)", output, stderr)
	}
}

func eventLines(output string) []domain.Event {
	var events []domain.Event
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var e domain.Event
		if json.Unmarshal([]byte(line), &e) == nil && e.Kind != "" {
			events = append(events, e)
		}
	}
	return events
}

// agentEnv is the environment of agent-a of pe-1: hl reads who watches from the token's scope.
func agentEnv(t *testing.T) []string {
	scope := base64.RawURLEncoding.EncodeToString([]byte("agent:pe-1/agent-a"))
	return []string{"HL_TOKEN=hls_" + scope + ".bWFj", "HL_STATE_DIR=" + t.TempDir()}
}

// A plain hl watch with no saved place prints every change held. The next one reads on from where
// the first stopped: what happened in between and nothing the first already printed.
func TestHLWatchReadsOnFromItsSavedPlaceWithoutRepeatsOrGaps(t *testing.T) {
	changes := newFakeChanges(statusEvent("exp-1", "COMPLETED", "agent-a"), statusEvent("exp-2", "FAILED", "agent-a"))
	server := watchServer(t, changes)
	env := agentEnv(t)

	code, out, stderr := runHLWatchEnv(t, env, server.URL, "--timeout", "10")
	if got := ids(eventLines(out)); code != 0 || fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("first watch: got = exit %v, ids %v, want = exit 0, [1 2] (stderr: %s)", code, got, stderr)
	}
	changes.append(statusEvent("exp-3", "EVICTED", "agent-a"))
	code, out, stderr = runHLWatchEnv(t, env, server.URL, "--timeout", "10")
	if got := ids(eventLines(out)); code != 0 || fmt.Sprint(got) != "[3]" {
		t.Errorf("second watch: got = exit %v, ids %v, want = exit 0, [3] (stderr: %s)", code, got, stderr)
	}
}

// A saved place the platform no longer holds, or never issued, resets the watch to the head with
// one watch.reset line, and the next watch reads on from there.
func TestHLWatchResetsToTheHeadWhenItsSavedPlaceIsGone(t *testing.T) {
	changes := newFakeChanges(statusEvent("exp-1", "COMPLETED", "agent-a"))
	server := watchServer(t, changes)
	env := agentEnv(t)
	dir := strings.TrimPrefix(env[1], "HL_STATE_DIR=")
	if err := os.WriteFile(filepath.Join(dir, "changes-pe-1-agent-a.json"), []byte(`{"after":5000}`), 0o644); err != nil {
		t.Fatalf("write cursor file: got = %v, want = nil", err)
	}

	code, out, stderr := runHLWatchEnv(t, env, server.URL, "--timeout", "10")
	if want := `{"detail":"changes after 5000 are no longer held: re-read state (hl situation)","kind":"watch.reset"}` + "\n"; code != 0 || out != want {
		t.Fatalf("watch from a place gone: got = exit %v, %q, want = exit 0, %q (stderr: %s)", code, out, want, stderr)
	}
	changes.append(statusEvent("exp-2", "COMPLETED", "agent-a"))
	code, out, stderr = runHLWatchEnv(t, env, server.URL, "--timeout", "10")
	if got := ids(eventLines(out)); code != 0 || fmt.Sprint(got) != "[2]" {
		t.Errorf("next watch: got = exit %v, ids %v, want = exit 0, [2] (stderr: %s)", code, got, stderr)
	}
}

// An explicit --after is the caller's statement of where to read from, and wins over the saved place.
func TestHLWatchExplicitAfterWinsOverTheSavedPlace(t *testing.T) {
	changes := newFakeChanges(statusEvent("exp-1", "COMPLETED", "agent-a"), statusEvent("exp-2", "COMPLETED", "agent-a"))
	server := watchServer(t, changes)
	env := agentEnv(t)
	if code, _, stderr := runHLWatchEnv(t, env, server.URL, "--timeout", "10"); code != 0 {
		t.Fatalf("first watch: got = exit %v, want = 0 (stderr: %s)", code, stderr)
	}
	code, out, stderr := runHLWatchEnv(t, env, server.URL, "--after", "1", "--timeout", "10")
	if got := ids(eventLines(out)); code != 0 || fmt.Sprint(got) != "[2]" {
		t.Errorf("watch with --after 1: got = exit %v, ids %v, want = exit 0, [2] (stderr: %s)", code, got, stderr)
	}
}

// A client that connects while LISTEN is down reads what is held and waits. What is appended
// before LISTEN is back woke nobody, so every stream reads again once it is.
func TestAStreamOpenedWhileNothingListenedReadsAgainOnceListenIsBack(t *testing.T) {
	changes := newFakeChanges(statusEvent("exp-1", "RUNNING", "agent-a"))
	changes.listenGate = make(chan struct{})
	client := dialWatch(t, watchServer(t, changes), url.Values{"platform_experiment_id": {"pe-1"}})
	if got := client.next(t).Value; got != "RUNNING" {
		t.Fatalf("change on connect: got = %v, want = RUNNING", got)
	}
	client.progress(t)
	changes.append(statusEvent("exp-1", "COMPLETED", "agent-a"))
	close(changes.listenGate)
	if got := client.next(t).Value; got != "COMPLETED" {
		t.Errorf("change appended while nothing listened: got = %v, want = COMPLETED", got)
	}
}
