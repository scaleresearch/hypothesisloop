// Package watchclient blocks until the platform reports an event on GET /watch, instead of
// polling for it. It speaks RFC 6455 itself (no third-party library) because it ships wherever
// `hl` does, including job images built on anything.
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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	TimeoutExitCode = 124
	UsageExitCode   = 2

	reconnectDelay = 2 * time.Second
)

var (
	readTimeout = 45 * time.Second
	// settleWindow is how long a watch without --until keeps collecting after its first event, so
	// events that arrive together (a replay after a reconnect, one transaction's several events)
	// come back as one batch.
	settleWindow = 2 * time.Second
)

// Closed means the connection ended, or the server was momentarily unable. Reconnect and replay.
type Closed struct{ msg string }

func (e *Closed) Error() string { return e.msg }

// Refused means the platform rejected this subscription. The request is wrong and will stay wrong.
type Refused struct {
	msg  string
	code int
}

func (e *Refused) Error() string { return e.msg }

// Options are hl watch's flags.
type Options struct {
	URL                string
	Experiment         string
	PlatformExperiment string
	Agent              string
	Kinds              string
	ShowSubscription   bool
	Until              string
	// Follow streams until the timeout instead of returning with the first batch.
	Follow bool
	// NoWait returns once the events already waiting have been printed.
	NoWait  bool
	Timeout time.Duration
	Since   int64
	// CursorFile, when set, is where the cursor is resumed from and saved to (see cursorState).
	CursorFile string

	// Stdout/Stderr let callers (and tests) capture output instead of the process streams.
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes the watch and returns the process exit code: 0 once a batch of events was printed
// (no --until), --until became true, or the window elapsed (no --until); 124 on timeout with
// --until still unmet; 2 on a usage error or a refused subscription; 1 when a watch without --until
// never connected, an event could not be written to Stdout, or the cursor could not be saved.
func Run(opts Options) int {
	if opts.URL == "" {
		fmt.Fprintln(opts.Stderr, "hl watch: no API URL given (set API_URL or pass --url)")
		return UsageExitCode
	}
	if opts.Timeout <= 0 {
		fmt.Fprintln(opts.Stderr, "hl watch: --timeout must be positive")
		return UsageExitCode
	}
	if opts.Experiment == "" && opts.PlatformExperiment == "" {
		fmt.Fprintln(opts.Stderr, "hl watch: one of --experiment or --platform-experiment is required")
		return UsageExitCode
	}

	w := &watch{opts: opts, cursor: opts.Since, seen: map[string]int64{}}
	if opts.Until != "" {
		fn, err := parseUntil(opts.Until)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "hl watch: %s\n", err)
			return UsageExitCode
		}
		w.satisfied = fn
	}
	if opts.CursorFile != "" && opts.Since == 0 {
		state, err := loadCursor(opts.CursorFile)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "hl watch: %s\n", err)
			return UsageExitCode
		}
		w.cursor, w.seen, w.resumed = state.Cursor, state.seen(), state.Cursor != 0
	}

	code := w.run()
	if code != UsageExitCode && opts.CursorFile != "" && w.printed {
		if err := saveCursor(opts.CursorFile, w.cursor, w.seen); err != nil {
			fmt.Fprintf(opts.Stderr, "hl watch: %s\n", err)
			return 1
		}
	}
	return code
}

// watch is one hl watch invocation: the cursor it resumes from, the identity and position of every
// event it printed (a replay repeats the events at its cursor), and when it stops.
type watch struct {
	opts      Options
	satisfied func(event map[string]any) bool
	cursor    int64
	seen      map[string]int64
	// resumed is true while the cursor came from the cursor file, so an unknown one falls back to
	// the snapshot once instead of failing.
	resumed  bool
	printed  bool
	settleBy time.Time
	writeErr error
}

func (w *watch) run() int {
	query := url.Values{}
	if w.opts.Experiment != "" {
		query.Set("experiment_id", w.opts.Experiment)
	}
	if w.opts.PlatformExperiment != "" {
		query.Set("platform_experiment_id", w.opts.PlatformExperiment)
	}
	if w.opts.Agent != "" {
		query.Set("agent", w.opts.Agent)
	}
	if w.opts.Kinds != "" {
		query.Set("kinds", w.opts.Kinds)
	}

	deadline := time.Now().Add(w.opts.Timeout)
	connected := false
	var connectErr error
	for time.Now().Before(w.until(deadline)) {
		if w.cursor != 0 {
			query.Set("since", strconv.FormatInt(w.cursor, 10))
		} else {
			query.Del("since")
		}
		ws, err := connect(w.opts.URL, "/watch?"+query.Encode(), deadline.Sub(time.Now()))
		if err != nil {
			var refused *Refused
			if errors.As(err, &refused) && refused.code == http.StatusGone && w.resumed {
				fmt.Fprintf(w.opts.Stderr, "hl watch: the platform does not know saved cursor %d; starting from the current state\n", w.cursor)
				w.cursor, w.seen, w.resumed = 0, map[string]int64{}, false
				continue
			}
			if errors.As(err, &refused) {
				fmt.Fprintf(w.opts.Stderr, "hl watch: %s\n", refused)
				return UsageExitCode
			}
			fmt.Fprintf(w.opts.Stderr, "hl watch: connect: %s\n", err)
			connectErr = err
			sleepUntil(w.until(deadline), reconnectDelay)
			continue
		}

		fmt.Fprintln(w.opts.Stderr, "hl watch: connected")
		connected = true
		done := w.stream(ws, deadline)
		ws.close()
		if w.writeErr != nil {
			fmt.Fprintf(w.opts.Stderr, "hl watch: write event: %s\n", w.writeErr)
			return 1
		}
		if done {
			return 0
		}
	}

	if w.satisfied != nil {
		fmt.Fprintf(w.opts.Stderr, "hl watch: timed out after %gs without: %s\n", w.opts.Timeout.Seconds(), w.opts.Until)
		return TimeoutExitCode
	}
	if !connected {
		fmt.Fprintf(w.opts.Stderr, "hl watch: never connected to %s in %gs: %v\n", w.opts.URL, w.opts.Timeout.Seconds(), connectErr)
		return 1
	}
	return 0
}

// until is when this watch stops: the deadline, or the end of the settle window once a batch has
// started.
func (w *watch) until(deadline time.Time) time.Time {
	if !w.settleBy.IsZero() {
		return minTime(deadline, w.settleBy)
	}
	return deadline
}

// stream reads frames from one connection until it closes, the watch stops or --until is
// satisfied. It returns true when the watch is done, false to reconnect or end on the deadline.
func (w *watch) stream(ws *webSocket, deadline time.Time) bool {
	// The server answers this frame after its replay, so in a NoWait watch the answer marks the
	// end of what was waiting.
	if w.opts.ShowSubscription || w.opts.NoWait {
		_ = ws.sendText("{}")
	}
	for time.Now().Before(w.until(deadline)) {
		line, err := ws.nextText(w.until(deadline))
		if err != nil {
			if !time.Now().Before(w.until(deadline)) {
				return !w.settleBy.IsZero()
			}
			fmt.Fprintf(w.opts.Stderr, "hl watch: stream ended (%s), reconnecting from cursor %d\n", err, w.cursor)
			sleepUntil(w.until(deadline), reconnectDelay)
			return false
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			fmt.Fprintln(w.opts.Stdout, line)
			continue
		}
		c, isEvent := event["cursor"].(float64)
		if _, caughtUp := event["subscription"]; caughtUp && w.opts.NoWait {
			return true
		}
		var identity []byte
		if isEvent {
			delete(event, "cursor")
			identity, _ = json.Marshal(event)
			if _, repeat := w.seen[string(identity)]; repeat {
				continue
			}
		}
		// The cursor moves past an event only once it is written, so one that was not is replayed.
		if _, err := fmt.Fprintln(w.opts.Stdout, line); err != nil {
			w.writeErr = err
			return true
		}
		if isEvent {
			position, _ := event["position"].(float64)
			w.seen[string(identity)] = int64(position)
			if int64(c) > w.cursor {
				w.cursor = int64(c)
			}
			w.printed = true
			if w.satisfied == nil && !w.opts.Follow && w.settleBy.IsZero() {
				w.settleBy = time.Now().Add(settleWindow)
			}
		}
		if w.satisfied != nil && w.satisfied(event) {
			return true
		}
	}
	return !w.settleBy.IsZero()
}

// cursorState is what the cursor file holds: the largest cursor printed, and the identity of each
// printed event at or above it, the only ones a resume can replay again.
type cursorState struct {
	Cursor int64             `json:"cursor"`
	Seen   []json.RawMessage `json:"seen"`
}

func (s cursorState) seen() map[string]int64 {
	seen := make(map[string]int64, len(s.Seen))
	for _, identity := range s.Seen {
		var event struct {
			Position int64 `json:"position"`
		}
		_ = json.Unmarshal(identity, &event)
		seen[string(identity)] = event.Position
	}
	return seen
}

func loadCursor(path string) (cursorState, error) {
	var state cursorState
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return state, fmt.Errorf("cursor file: %w", err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("cursor file: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("cursor file %s: %w", path, err)
	}
	return state, nil
}

func saveCursor(path string, cursor int64, seen map[string]int64) error {
	state := cursorState{Cursor: cursor, Seen: []json.RawMessage{}}
	for identity, position := range seen {
		if position >= cursor {
			state.Seen = append(state.Seen, json.RawMessage(identity))
		}
	}
	sort.Slice(state.Seen, func(i, j int) bool { return string(state.Seen[i]) < string(state.Seen[j]) })
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("save cursor: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("save cursor: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("save cursor: %w", err)
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func sleepUntil(deadline time.Time, d time.Duration) {
	time.Sleep(min(d, time.Until(deadline)))
}

// parseUntil turns "status in A,B,C" into a predicate over one job or platform experiment status
// event (their values never overlap: jobs are upper case, runs lower case), the only form hl watch
// understands. A wait that can never end is the failure this tool exists to remove, so an
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
		return (kind == "experiment.status" || kind == "platform_experiment.status") && wanted[value]
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
		return nil, &Refused{msg: msg, code: code}
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

// nextText returns the next text frame, answering pings and skipping everything else. Every frame
// renews the read timeout, so the server's pings keep a quiet stream open.
func (ws *webSocket) nextText(deadline time.Time) (string, error) {
	for {
		ws.setReadDeadline(minTime(deadline, time.Now().Add(readTimeout)))
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
