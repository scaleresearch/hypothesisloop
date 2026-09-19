package watchclient

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
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
	url := serveOneFrame(t, `{"kind":"experiment.status","value":"COMPLETED","cursor":1}`, 0, "")

	var stdout, stderr bytes.Buffer
	code := Run(Options{
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
	code := Run(Options{URL: "http://localhost:1", Timeout: time.Second, Stdout: &stdout, Stderr: &stderr})
	if code != UsageExitCode {
		t.Fatalf("exit code = %d, want %d", code, UsageExitCode)
	}
}

func TestRunRefused(t *testing.T) {
	url := serveOneFrame(t, "", http.StatusBadRequest, `{"error":"unknown kind"}`)

	var stdout, stderr bytes.Buffer
	code := Run(Options{
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
		_, _ = conn.Write(serverTextFrame(`{"kind":"experiment.status","value":"COMPLETED","cursor":1}`))
		time.Sleep(time.Second)
	}()

	var stdout, stderr bytes.Buffer
	code := Run(Options{
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
	code := Run(Options{URL: "ws://" + ln.Addr().String(), Experiment: "exp-1", Timeout: 300 * time.Millisecond, Stdout: &stdout, Stderr: &stderr})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "stream ended") {
		t.Fatalf("the window ending was reported as a dropped stream: %s", stderr.String())
	}
}

// A reconnect resumes from the largest cursor seen, and the server replays from it, the event at
// it included. That repeat must not reach the output a second time.
func TestAReconnectResumesFromTheLargestCursorAndPrintsNoEventTwice(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	queued := `{"kind":"experiment.status","subject":"exp-1","value":"QUEUED","position":12,"cursor":12}`
	running := `{"kind":"experiment.status","subject":"exp-1","value":"RUNNING","position":12,"cursor":9}`
	completed := `{"kind":"experiment.status","subject":"exp-1","value":"COMPLETED","position":15,"cursor":14}`
	sinces := make(chan string, 2)
	go func() {
		for _, frames := range [][]string{
			{queued, running},
			{`{"kind":"experiment.status","subject":"exp-1","value":"QUEUED","position":12,"cursor":11}`, completed},
		} {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			req, err := http.ReadRequest(bufio.NewReader(conn))
			if err != nil {
				conn.Close()
				return
			}
			sinces <- req.URL.Query().Get("since")
			_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
				"Sec-WebSocket-Accept: " + computeAccept(req.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"))
			for _, frame := range frames {
				_, _ = conn.Write(serverTextFrame(frame))
			}
			_, _ = conn.Write([]byte{0x88, 0x00})
			conn.Close()
		}
	}()

	var stdout, stderr bytes.Buffer
	code := Run(Options{
		URL:        "ws://" + ln.Addr().String(),
		Experiment: "exp-1",
		Until:      "status in COMPLETED",
		Timeout:    10 * time.Second,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if first, second := <-sinces, <-sinces; first != "" || second != "12" {
		t.Errorf("since on connect and on reconnect: got = %q, %q, want = none, then the largest cursor 12", first, second)
	}
	if got, want := stdout.String(), queued+"\n"+running+"\n"+completed+"\n"; got != want {
		t.Errorf("output: got = %q, want = %q", got, want)
	}
}
