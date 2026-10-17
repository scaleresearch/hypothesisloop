package watchclient

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseUntil(t *testing.T) {
	fn, err := parseUntil("status in COMPLETED,FAILED,EVICTED")
	if err != nil {
		t.Fatal(err)
	}
	if !fn(map[string]any{"kind": "experiment.status", "value": "COMPLETED"}) {
		t.Fatal("expected match on COMPLETED")
	}
	if fn(map[string]any{"kind": "experiment.status", "value": "QUEUED"}) {
		t.Fatal("did not expect match on QUEUED")
	}
	if fn(map[string]any{"kind": "quota.snapshot", "value": "COMPLETED"}) {
		t.Fatal("wrong kind should not match")
	}
	running, _ := parseUntil("status in running")
	if !running(map[string]any{"kind": "platform_experiment.status", "value": "running"}) {
		t.Fatal("expected match on a platform experiment reaching running")
	}
}

func TestParseUntilRejectsOtherForms(t *testing.T) {
	for _, expr := range []string{"", "status = COMPLETED", "foo in bar", "status in "} {
		if _, err := parseUntil(expr); err == nil {
			t.Fatalf("expected error for %q", expr)
		}
	}
}

// --- a minimal test server that speaks just enough RFC 6455 to exercise the client ---------------

// serveOneFrame accepts a raw TCP/HTTP upgrade, sends one text frame, then closes.
func serveOneFrame(t *testing.T, message string, statusCode int, body string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		if statusCode != 0 {
			resp := fmt.Sprintf("HTTP/1.1 %d Refused\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
				statusCode, len(body), body)
			_, _ = conn.Write([]byte(resp))
			return
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		accept := computeAccept(key)
		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
		_, _ = conn.Write([]byte(resp))

		frame := serverTextFrame(message)
		_, _ = conn.Write(frame)
		// Send a close frame so the client sees a clean end rather than an EOF error masking it.
		_, _ = conn.Write([]byte{0x88, 0x00})
	}()

	return "ws://" + ln.Addr().String()
}

func computeAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func serverTextFrame(payload string) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0x81) // fin + text opcode
	length := len(payload)
	switch {
	case length <= 125:
		buf.WriteByte(byte(length))
	case length <= 0xFFFF:
		buf.WriteByte(126)
		lb := make([]byte, 2)
		binary.BigEndian.PutUint16(lb, uint16(length))
		buf.Write(lb)
	default:
		buf.WriteByte(127)
		lb := make([]byte, 8)
		binary.BigEndian.PutUint64(lb, uint64(length))
		buf.Write(lb)
	}
	buf.WriteString(payload)
	return buf.Bytes()
}

func TestRunReceivesEventAndSatisfiesUntil(t *testing.T) {
	url := serveOneFrame(t, `{"id":1,"kind":"experiment.status","value":"COMPLETED"}`, 0, "")

	var stdout, stderr bytes.Buffer
	code := Run(Options{
		Token:      "test-token",
		URL:        url,
		Experiment: "exp-1",
		Until:      "status in COMPLETED,FAILED,EVICTED",
		Timeout:    5 * time.Second,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "COMPLETED") {
		t.Fatalf("event not printed: %s", stdout.String())
	}
}

func TestRunMissingTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(Options{Token: "test-token", URL: "http://localhost:1", Timeout: time.Second, Stdout: &stdout, Stderr: &stderr})
	if code != UsageExitCode {
		t.Fatalf("exit code = %d, want %d", code, UsageExitCode)
	}
}

func TestRunWithoutATokenIsAUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(Options{URL: "http://localhost:1", Experiment: "exp-1", Timeout: time.Second, Stdout: &stdout, Stderr: &stderr})
	if code != UsageExitCode || !strings.Contains(stderr.String(), "HL_TOKEN") {
		t.Fatalf("exit code = %d, stderr = %q, want %d naming HL_TOKEN", code, stderr.String(), UsageExitCode)
	}
}

// The missing address was answered with a --url flag hl watch does not have.
func TestRunWithoutAnAddressNamesTheVariableAndNoFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(Options{Token: "test-token", Experiment: "exp-1", Timeout: time.Second, Stdout: &stdout, Stderr: &stderr})
	if code != UsageExitCode || stderr.String() != "hl watch: no API URL given (set API_URL)\n" {
		t.Fatalf("exit code = %d, stderr = %q, want %d naming API_URL alone", code, stderr.String(), UsageExitCode)
	}
}

// The token travels as the second subprotocol, where a browser can put it too.
func TestTheTokenIsOfferedAsASubprotocol(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	offered := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		offered <- req.Header.Get("Sec-WebSocket-Protocol") + "|" + req.Header.Get("Authorization")
		_, _ = conn.Write([]byte("HTTP/1.1 400 Refused\r\nContent-Length: 0\r\n\r\n"))
	}()
	var stdout, stderr bytes.Buffer
	Run(Options{Token: "the-token", URL: "ws://" + ln.Addr().String(), Experiment: "exp-1", Timeout: time.Second, Stdout: &stdout, Stderr: &stderr})
	if got := <-offered; got != "hypothesisloop.watch, the-token|" {
		t.Fatalf("handshake offered %q, want the subprotocol then the token and no Authorization header", got)
	}
}

func TestRunRefused(t *testing.T) {
	url := serveOneFrame(t, "", http.StatusBadRequest, `{"message":"unknown kind"}`)

	var stdout, stderr bytes.Buffer
	code := Run(Options{
		Token:      "test-token",
		URL:        url,
		Experiment: "exp-1",
		Timeout:    5 * time.Second,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if code != UsageExitCode {
		t.Fatalf("exit code = %d, want %d, stderr = %s", code, UsageExitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown kind") {
		t.Fatalf("refusal message not surfaced: %s", stderr.String())
	}
}

func TestAWatchThatNeverConnectedFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := Run(Options{Token: "test-token", URL: "ws://" + addr, Experiment: "exp-1", Timeout: 300 * time.Millisecond, Stdout: &stdout, Stderr: &stderr})
	if code != 1 || !strings.Contains(stderr.String(), "never connected") {
		t.Fatalf("got = exit %d, stderr %q, want = exit 1 naming the outage", code, stderr.String())
	}
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Fatalf("returned after %s, want the whole window", waited)
	}
	if attempts := strings.Count(stderr.String(), "connect:"); attempts > 3 {
		t.Fatalf("got = %d connect attempts in 300ms, want = no spinning", attempts)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

func TestAnEventThatCouldNotBeWrittenDoesNotMoveTheCursor(t *testing.T) {
	url := serveOneFrame(t, `{"id":7,"kind":"experiment.status","subject":"exp-1","value":"COMPLETED"}`, 0, "")
	cursorFile := filepath.Join(t.TempDir(), "cursor.json")

	var stderr bytes.Buffer
	code := Run(Options{Token: "test-token", URL: url, PlatformExperiment: "pe-1", Agent: "agent-a", Timeout: 5 * time.Second,
		CursorFile: cursorFile, Stdout: brokenWriter{}, Stderr: &stderr})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(cursorFile); !os.IsNotExist(err) {
		t.Fatalf("cursor file: got = %v, want = not written", err)
	}
}

func TestRunTimesOutWithoutUntil(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Nothing accepts connections beyond the listener, so every dial attempt just times out —
	// exercising the "connection failed, retry until deadline" path without asserting timing.
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := Run(Options{
		Token:      "test-token",
		URL:        "ws://" + ln.Addr().String(),
		Experiment: "exp-1",
		Timeout:    300 * time.Millisecond,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took too long: %s", time.Since(start))
	}
	_ = code // either exit code is acceptable here; this test only guards against hanging
}

func TestPingsKeepAQuietStreamOpen(t *testing.T) {
	defer func(d time.Duration) { readTimeout = d }(readTimeout)
	readTimeout = 200 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + computeAccept(req.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"))
		for i := 0; i < 8; i++ {
			time.Sleep(100 * time.Millisecond)
			_, _ = conn.Write([]byte{0x89, 0x00})
		}
		_, _ = conn.Write(serverTextFrame(`{"id":1,"kind":"experiment.status","value":"COMPLETED"}`))
		time.Sleep(time.Second)
	}()

	var stdout, stderr bytes.Buffer
	code := Run(Options{
		Token:      "test-token",
		URL:        "ws://" + ln.Addr().String(),
		Experiment: "exp-1",
		Until:      "status in COMPLETED",
		Timeout:    5 * time.Second,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "stream ended") {
		t.Fatalf("a stream carrying only pings was dropped: %s", stderr.String())
	}
}

func TestTheWatchWindowEndingIsNotAReconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + computeAccept(req.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"))
		time.Sleep(time.Second)
	}()

	var stdout, stderr bytes.Buffer
	code := Run(Options{Token: "test-token", URL: "ws://" + ln.Addr().String(), Experiment: "exp-1", Timeout: 300 * time.Millisecond, Stdout: &stdout, Stderr: &stderr})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "stream ended") {
		t.Fatalf("the window ending was reported as a dropped stream: %s", stderr.String())
	}
}

// accept answers one WebSocket upgrade on ln and returns the request and the connection.
func accept(t *testing.T, ln net.Listener) (*http.Request, net.Conn) {
	conn, err := ln.Accept()
	if err != nil {
		return nil, nil
	}
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		conn.Close()
		return nil, nil
	}
	_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + computeAccept(req.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"))
	return req, conn
}

// A reconnect reads on from the id of the last change printed, so nothing is printed twice and
// nothing is skipped.
func TestAReconnectReadsOnFromTheLastChangePrinted(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	queued := `{"id":12,"kind":"experiment.status","subject":"exp-1","value":"QUEUED","occurred_at":"2026-10-06T21:31:44Z"}`
	running := `{"id":13,"kind":"experiment.status","subject":"exp-1","value":"RUNNING"}`
	completed := `{"id":15,"kind":"experiment.status","subject":"exp-1","value":"COMPLETED"}`
	afters := make(chan string, 2)
	go func() {
		for _, frames := range [][]string{{queued, running}, {completed}} {
			req, conn := accept(t, ln)
			if conn == nil {
				return
			}
			afters <- req.URL.Query().Get("after")
			for _, frame := range frames {
				_, _ = conn.Write(serverTextFrame(frame))
			}
			_, _ = conn.Write([]byte{0x88, 0x00})
			conn.Close()
		}
	}()

	var stdout, stderr bytes.Buffer
	code := Run(Options{Token: "test-token", URL: "ws://" + ln.Addr().String(), Experiment: "exp-1", Until: "status in COMPLETED",
		Timeout: 10 * time.Second, Stdout: &stdout, Stderr: &stderr})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if first, second := <-afters, <-afters; first != "" || second != "13" {
		t.Errorf("after on connect and on reconnect: got = %q, %q, want = none, then 13", first, second)
	}
	if got, want := stdout.String(), queued+"\n"+running+"\n"+completed+"\n"; got != want {
		t.Errorf("output: got = %q, want = %q", got, want)
	}
}

// Without --until a watch returns with its first batch: what arrives within the settle window after
// the first change, and nothing after it.
func TestWithoutUntilTheFirstBatchIsReturnedAfterTheSettleWindow(t *testing.T) {
	defer func(d time.Duration) { settleWindow = d }(settleWindow)
	settleWindow = 300 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	first := `{"id":10,"kind":"experiment.status","value":"COMPLETED"}`
	second := `{"id":11,"kind":"comment.new","subject":"hyp-1"}`
	late := `{"id":12,"kind":"experiment.status","value":"FAILED"}`
	go func() {
		_, conn := accept(t, ln)
		if conn == nil {
			return
		}
		defer conn.Close()
		time.Sleep(200 * time.Millisecond)
		_, _ = conn.Write(serverTextFrame(first))
		time.Sleep(100 * time.Millisecond)
		_, _ = conn.Write(serverTextFrame(second))
		time.Sleep(time.Second)
		_, _ = conn.Write(serverTextFrame(late))
		time.Sleep(time.Second)
	}()

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := Run(Options{Token: "test-token", URL: "ws://" + ln.Addr().String(), PlatformExperiment: "pe-1", Agent: "agent-a",
		Timeout: 10 * time.Second, Stdout: &stdout, Stderr: &stderr})
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), first+"\n"+second+"\n"; got != want {
		t.Errorf("output: got = %q, want = %q", got, want)
	}
	if elapsed > 1200*time.Millisecond {
		t.Errorf("returned after %s, want = soon after the settle window", elapsed)
	}
}

// A watch whose timeout ends inside the settle window still returns what it read and saves where
// it stands.
func TestATimeoutInsideTheSettleWindowReturnsTheChangesAndSavesTheCursor(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	change := `{"id":10,"kind":"experiment.status","value":"COMPLETED"}`
	go func() {
		_, conn := accept(t, ln)
		if conn == nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write(serverTextFrame(change))
		time.Sleep(2 * time.Second)
	}()

	cursorFile := filepath.Join(t.TempDir(), "cursor.json")
	var stdout, stderr bytes.Buffer
	code := Run(Options{Token: "test-token", URL: "ws://" + ln.Addr().String(), PlatformExperiment: "pe-1", Agent: "agent-a",
		Timeout: 500 * time.Millisecond, CursorFile: cursorFile, Stdout: &stdout, Stderr: &stderr})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if got, want := stdout.String(), change+"\n"; got != want {
		t.Errorf("output: got = %q, want = %q", got, want)
	}
	if state, err := loadCursor(cursorFile); err != nil || state.After != 10 {
		t.Errorf("saved cursor: got = %d (%v), want = 10", state.After, err)
	}
}

// A NoWait watch returns at the first caught-up progress frame: what was waiting is printed, and
// the place saved is the progress frame's, past changes the subscription left out.
func TestNoWaitReturnsOnceCaughtUp(t *testing.T) {
	change := `{"id":10,"kind":"experiment.status","value":"COMPLETED"}`
	for _, waiting := range []string{change, ""} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			_, conn := accept(t, ln)
			if conn == nil {
				return
			}
			defer conn.Close()
			_, _ = conn.Write(serverTextFrame(`{"subscription":{"after":0}}`))
			if waiting != "" {
				_, _ = conn.Write(serverTextFrame(waiting))
			}
			_, _ = conn.Write(serverTextFrame(`{"next":14,"caught_up":true}`))
			time.Sleep(5 * time.Second)
		}()

		cursorFile := filepath.Join(t.TempDir(), "cursor.json")
		var stdout, stderr bytes.Buffer
		start := time.Now()
		code := Run(Options{Token: "test-token", URL: "ws://" + ln.Addr().String(), PlatformExperiment: "pe-1", Agent: "agent-a", NoWait: true,
			Timeout: 10 * time.Second, CursorFile: cursorFile, Stdout: &stdout, Stderr: &stderr})
		if code != 0 {
			t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("returned after %s, want = at once", elapsed)
		}
		want := ""
		if waiting != "" {
			want = waiting + "\n"
		}
		if got := stdout.String(); got != want {
			t.Errorf("output: got = %q, want = %q", got, want)
		}
		if state, err := loadCursor(cursorFile); err != nil || state.After != 14 {
			t.Errorf("saved cursor: got = %d (%v), want = 14", state.After, err)
		}
	}
}

// A watch with Wake holds a change of another kind back, and the place it saves with it, so the
// next watch reads it again, and hands it over ahead of the change that ends the wait.
func TestAWakeWatchHoldsOtherKindsUntilAWakingOneArrives(t *testing.T) {
	defer func(d time.Duration) { settleWindow = d }(settleWindow)
	settleWindow = 100 * time.Millisecond
	finding := `{"id":10,"kind":"finding.new","subject":"hyp-1"}`
	finished := `{"id":11,"kind":"agent.finished","subject":"agent-a"}`
	others := `{"id":12,"kind":"agent.finished","subject":"agent-b"}`
	for _, tc := range []struct {
		frames    []string
		want      string
		wantAfter int64
	}{
		{[]string{finding, `{"next":10,"caught_up":true}`}, "", 0},
		{[]string{others, `{"next":12,"caught_up":true}`}, "", 0},
		{[]string{finding, finding, finished}, finding + "\n" + finished + "\n", 11},
	} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			_, conn := accept(t, ln)
			if conn == nil {
				return
			}
			defer conn.Close()
			for _, frame := range tc.frames {
				_, _ = conn.Write(serverTextFrame(frame))
			}
			time.Sleep(2 * time.Second)
		}()

		cursorFile := filepath.Join(t.TempDir(), "cursor.json")
		var stdout, stderr bytes.Buffer
		code := Run(Options{Token: "test-token", URL: "ws://" + ln.Addr().String(), PlatformExperiment: "pe-1", Agent: "agent-a",
			Wake: func(kind, subject, _ string) bool { return kind == "agent.finished" && subject == "agent-a" }, Timeout: 500 * time.Millisecond, CursorFile: cursorFile, Stdout: &stdout, Stderr: &stderr})
		if code != 0 || stdout.String() != tc.want {
			t.Fatalf("exit code = %d, output = %q, want = %q, stderr = %s", code, stdout.String(), tc.want, stderr.String())
		}
		if state, err := loadCursor(cursorFile); err != nil || state.After != tc.wantAfter {
			t.Errorf("saved cursor: got = %d (%v), want = %d", state.After, err, tc.wantAfter)
		}
	}
}

// A saved place the platform no longer holds is answered 410: the watch reads on from the head,
// saves it, and prints one watch.reset line.
func TestASavedPlaceNoLongerHeldResetsToTheHead(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	afters := make(chan string, 2)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err == nil {
			afters <- req.URL.Query().Get("after")
			body := `{"message":"changes after 5 are no longer held; re-read state and watch from head"}`
			_, _ = conn.Write([]byte(fmt.Sprintf("HTTP/1.1 410 Gone\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)))
		}
		conn.Close()
		req, conn = accept(t, ln)
		if conn == nil {
			return
		}
		defer conn.Close()
		afters <- req.URL.Query().Get("after")
		_, _ = conn.Write(serverTextFrame(`{"subscription":{"after":90}}`))
		_, _ = conn.Write(serverTextFrame(`{"next":90,"caught_up":true}`))
		time.Sleep(2 * time.Second)
	}()

	cursorFile := filepath.Join(t.TempDir(), "cursor.json")
	if err := saveCursor(cursorFile, 5); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(Options{Token: "test-token", URL: "ws://" + ln.Addr().String(), PlatformExperiment: "pe-1", Agent: "agent-a",
		Timeout: 5 * time.Second, CursorFile: cursorFile, Stdout: &stdout, Stderr: &stderr})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if first, second := <-afters, <-afters; first != "5" || second != "head" {
		t.Errorf("after: got = %q, %q, want = 5, then head", first, second)
	}
	if got, want := stdout.String(), `{"detail":"changes after 5 are no longer held: re-read state (hl situation)","kind":"watch.reset"}`+"\n"; got != want {
		t.Errorf("output: got = %q, want = %q", got, want)
	}
	if state, err := loadCursor(cursorFile); err != nil || state.After != 90 {
		t.Errorf("saved cursor: got = %d (%v), want = 90", state.After, err)
	}
}
