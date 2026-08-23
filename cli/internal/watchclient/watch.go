// Package watchclient is a native Go port of agents/coordinator/experiments/hl-watch: block until
// the platform reports an event on GET /watch, instead of polling for it. It speaks RFC 6455
// itself (no third-party library) for the same reason the Python original does — the platform's
// own API images are whatever a workload happens to be built on, and this ships wherever `hl`
// does.
package watchclient

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	TimeoutExitCode = 124
	UsageExitCode   = 2

	readTimeout    = 45 * time.Second
	reconnectDelay = 2 * time.Second
)

// Closed means the connection ended, or the server was momentarily unable. Reconnect and replay.
type Closed struct{ msg string }

func (e *Closed) Error() string { return e.msg }

// Refused means the platform rejected this subscription. The request is wrong and will stay wrong.
type Refused struct{ msg string }

func (e *Refused) Error() string { return e.msg }

// Options mirrors hl-watch's flags.
type Options struct {
	URL                 string
	Experiment          string
	PlatformExperiment  string
	Agent               string
	Kinds               string
	ShowSubscription    bool
	Until               string
	Timeout             time.Duration
	Since               int64

	// Stdout/Stderr let callers (and tests) capture output instead of the process streams.
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes the watch loop and returns the process exit code, matching hl-watch's contract:
// 0 on --until becoming true (or the window elapsing without --until), 124 on timeout with
// --until still unmet, 2 on a usage error or a refused subscription.
func Run(opts Options) int {
	if opts.URL == "" {
		fmt.Fprintln(opts.Stderr, "hl watch: no API URL given (set API_URL or pass --url)")
		return UsageExitCode
	}
	if opts.Experiment == "" && opts.PlatformExperiment == "" {
		fmt.Fprintln(opts.Stderr, "hl watch: one of --experiment or --platform-experiment is required")
		return UsageExitCode
	}

	var satisfied func(event map[string]any) bool
	if opts.Until != "" {
		fn, err := parseUntil(opts.Until)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "hl watch: %s\n", err)
			return UsageExitCode
		}
		satisfied = fn
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 900 * time.Second
	}

	query := url.Values{}
	if opts.Experiment != "" {
		query.Set("experiment_id", opts.Experiment)
	}
	if opts.PlatformExperiment != "" {
		query.Set("platform_experiment_id", opts.PlatformExperiment)
	}
	if opts.Agent != "" {
		query.Set("agent", opts.Agent)
	}
	if opts.Kinds != "" {
		query.Set("kinds", opts.Kinds)
	}

	deadline := time.Now().Add(timeout)
	cursor := opts.Since
	for time.Now().Before(deadline) {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		if cursor != 0 {
			q.Set("since", strconv.FormatInt(cursor, 10))
		}
		path := "/watch?" + q.Encode()

		ws, err := connect(opts.URL, path, deadline.Sub(time.Now()))
		if err != nil {
			var refused *Refused
			if errors.As(err, &refused) {
				fmt.Fprintf(opts.Stderr, "hl watch: %s\n", refused)
				return UsageExitCode
			}
			fmt.Fprintf(opts.Stderr, "hl watch: connect: %s\n", err)
			sleepUntil(deadline, reconnectDelay)
			continue
		}

		fmt.Fprintln(opts.Stderr, "hl watch: connected")
		done, code := streamOne(ws, opts, deadline, satisfied, &cursor)
		ws.close()
		if done {
			return code
		}
	}

	if satisfied != nil {
		fmt.Fprintf(opts.Stderr, "hl watch: timed out after %gs without: %s\n", timeout.Seconds(), opts.Until)
		return TimeoutExitCode
	}
	return 0
}

// streamOne reads frames from one connection until it closes, the deadline passes, or --until is
// satisfied. Returns (true, code) when the caller should stop entirely, (false, _) to reconnect.
func streamOne(ws *webSocket, opts Options, deadline time.Time, satisfied func(map[string]any) bool, cursor *int64) (bool, int) {
	if opts.ShowSubscription {
		_ = ws.sendText("{}")
	}
	for time.Now().Before(deadline) {
		ws.setReadDeadline(minTime(deadline, time.Now().Add(readTimeout)))
		line, err := ws.nextText()
		if err != nil {
			fmt.Fprintf(opts.Stderr, "hl watch: stream ended (%s) — reconnecting from cursor %d\n", err, *cursor)
			sleepUntil(deadline, reconnectDelay)
			return false, 0
		}
		fmt.Fprintln(opts.Stdout, line)
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if c, ok := event["cursor"].(float64); ok && int64(c) > *cursor {
			*cursor = int64(c)
		}
		if satisfied != nil && satisfied(event) {
			return true, 0
		}
	}
	return false, 0
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func sleepUntil(deadline time.Time, d time.Duration) {
	if time.Now().Add(d).After(deadline) {
		return
	}
	time.Sleep(d)
}

// parseUntil turns "status in A,B,C" into a predicate over one event, the only form hl-watch
// understands — a wait that can never end is the failure this tool exists to remove, so an
// expression that isn't this shape is a usage error rather than a condition that quietly never
// fires.
func parseUntil(expr string) (func(map[string]any) bool, error) {
	fields := strings.Fields(expr)
	if len(fields) < 3 || fields[0] != "status" || fields[1] != "in" {
		return nil, errors.New("--until must read: status in VALUE[,VALUE...]")
	}
	rest := strings.Join(fields[2:], " ")
	wanted := map[string]bool{}
	for _, v := range strings.Split(rest, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			wanted[v] = true
		}
	}
	if len(wanted) == 0 {
		return nil, errors.New("--until names no values")
	}
	return func(event map[string]any) bool {
		kind, _ := event["kind"].(string)
		value, _ := event["value"].(string)
		return kind == "experiment.status" && wanted[value]
	}, nil
}

// --- minimal RFC 6455 client -----------------------------------------------------------------

type webSocket struct {
	conn net.Conn
	buf  []byte
}

func connect(base, path string, budget time.Duration) (*webSocket, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	secure := u.Scheme == "https" || u.Scheme == "wss"
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if secure {
			port = "443"
		} else {
			port = "80"
		}
	}
	dialTimeout := budget
	if dialTimeout <= 0 || dialTimeout > readTimeout {
		dialTimeout = readTimeout
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), dialTimeout)
	if err != nil {
		return nil, err
	}
	if secure {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tlsConn
	}

	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)

	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + net.JoinHostPort(host, port) + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}

	ws := &webSocket{conn: conn}
	ws.setReadDeadline(time.Now().Add(dialTimeout))
	head, err := ws.readUntilHeaderEnd()
	if err != nil {
		conn.Close()
		return nil, err
	}
	statusLine := strings.SplitN(string(head), "\r\n", 2)[0]
	if strings.Contains(statusLine, " 101 ") {
		return ws, nil
	}
	code := statusCode(statusLine)
	detail := ws.readRefusalBody(head)
	conn.Close()
	msg := fmt.Sprintf("the platform refused this subscription: %s%s", strings.TrimSpace(statusLine), detail)
	if code >= 400 && code < 500 {
		return nil, &Refused{msg: msg}
	}
	return nil, &Closed{msg: fmt.Sprintf("handshake failed: %s%s", strings.TrimSpace(statusLine), detail)}
}

func statusCode(line string) int {
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return n
}

func (ws *webSocket) readUntilHeaderEnd() ([]byte, error) {
	for {
		if i := indexHeaderEnd(ws.buf); i >= 0 {
			head := ws.buf[:i]
			ws.buf = ws.buf[i+4:]
			return head, nil
		}
		chunk := make([]byte, 65536)
		n, err := ws.conn.Read(chunk)
		if n > 0 {
			ws.buf = append(ws.buf, chunk[:n]...)
		}
		if err != nil {
			return nil, fmt.Errorf("server closed during the handshake: %w", err)
		}
	}
}

func indexHeaderEnd(b []byte) int {
	return strings.Index(string(b), "\r\n\r\n")
}

// readRefusalBody returns " — <message>" for a refusal's JSON {"error": "..."} body, which is the
// only thing that says which part of the request was wrong.
func (ws *webSocket) readRefusalBody(head []byte) string {
	length := 0
	for _, line := range strings.Split(string(head), "\r\n")[1:] {
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "content-length") {
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				length = n
			}
		}
	}
	var body []byte
	if length > 0 {
		for len(ws.buf) < length {
			chunk := make([]byte, 65536)
			n, err := ws.conn.Read(chunk)
			ws.buf = append(ws.buf, chunk[:n]...)
			if err != nil {
				break
			}
		}
		if len(ws.buf) > length {
			body = ws.buf[:length]
		} else {
			body = ws.buf
		}
	} else {
		body = ws.buf
		for {
			chunk := make([]byte, 65536)
			n, err := ws.conn.Read(chunk)
			if n > 0 {
				body = append(body, chunk[:n]...)
			}
			if err != nil {
				break
			}
		}
	}
	text := strings.TrimSpace(string(body))
	var payload struct {
		Error string `json:"error"`
	}
	message := text
	if json.Unmarshal(body, &payload) == nil && payload.Error != "" {
		message = payload.Error
	}
	if message == "" {
		return ""
	}
	return " — " + message
}

func (ws *webSocket) setReadDeadline(t time.Time) {
	_ = ws.conn.SetReadDeadline(t)
}

func (ws *webSocket) readExactly(n int) ([]byte, error) {
	for len(ws.buf) < n {
		chunk := make([]byte, 65536)
		read, err := ws.conn.Read(chunk)
		if read > 0 {
			ws.buf = append(ws.buf, chunk[:read]...)
		}
		if err != nil {
			if len(ws.buf) >= n {
				break
			}
			return nil, &Closed{msg: err.Error()}
		}
	}
	out := ws.buf[:n]
	ws.buf = ws.buf[n:]
	return out, nil
}

func (ws *webSocket) sendFrame(opcode byte, payload []byte) error {
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	var header []byte
	header = append(header, 0x80|opcode)
	length := len(payload)
	switch {
	case length <= 125:
		header = append(header, 0x80|byte(length))
	case length <= 0xFFFF:
		header = append(header, 0x80|126)
		lb := make([]byte, 2)
		binary.BigEndian.PutUint16(lb, uint16(length))
		header = append(header, lb...)
	default:
		header = append(header, 0x80|127)
		lb := make([]byte, 8)
		binary.BigEndian.PutUint64(lb, uint64(length))
		header = append(header, lb...)
	}
	masked := make([]byte, length)
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	_, err := ws.conn.Write(append(append(header, mask...), masked...))
	return err
}

func (ws *webSocket) sendText(text string) error {
	return ws.sendFrame(0x1, []byte(text))
}

// nextText returns the next text frame, answering pings and skipping everything else.
func (ws *webSocket) nextText() (string, error) {
	for {
		head, err := ws.readExactly(2)
		if err != nil {
			return "", err
		}
		opcode := head[0] & 0x0F
		length := int(head[1] & 0x7F)
		if length == 126 {
			lb, err := ws.readExactly(2)
			if err != nil {
				return "", err
			}
			length = int(binary.BigEndian.Uint16(lb))
		} else if length == 127 {
			lb, err := ws.readExactly(8)
			if err != nil {
				return "", err
			}
			length = int(binary.BigEndian.Uint64(lb))
		}
		var payload []byte
		if head[1]&0x80 != 0 {
			mask, err := ws.readExactly(4)
			if err != nil {
				return "", err
			}
			raw, err := ws.readExactly(length)
			if err != nil {
				return "", err
			}
			payload = make([]byte, length)
			for i, b := range raw {
				payload[i] = b ^ mask[i%4]
			}
		} else {
			payload, err = ws.readExactly(length)
			if err != nil {
				return "", err
			}
		}
		switch opcode {
		case 0x8:
			return "", &Closed{msg: "server closed the stream"}
		case 0x9:
			_ = ws.sendFrame(0xA, payload)
			continue
		case 0x1:
			return string(payload), nil
		}
	}
}

func (ws *webSocket) close() {
	_ = ws.sendFrame(0x8, nil)
	_ = ws.conn.Close()
}
