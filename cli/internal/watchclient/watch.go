// Package watchclient blocks until the platform reports a change on GET /watch, instead of
// polling for it. It speaks RFC 6455 itself (no third-party library) because it ships wherever
// `hl` does.
package watchclient

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
)

const (
	TimeoutExitCode = 124
	UsageExitCode   = 2

	reconnectDelay = 2 * time.Second
)

var (
	// readTimeout is how long a stream may be silent before it is read as dead: two of the
	// server's pings missed, and a little.
	readTimeout = 2*platformapi.WatchPingInterval + 5*time.Second
	// settleWindow is how long a watch without --until keeps collecting after its first change, so
	// changes that arrive together come back as one batch.
	settleWindow = 2 * time.Second
)

// Closed means the connection ended, or the server was momentarily unable. Reconnect and read on.
type Closed struct{ msg string }

func (e *Closed) Error() string { return e.msg }

// Refused means the platform rejected this subscription. The request is wrong and will stay wrong.
type Refused struct {
	msg    string
	reason string
}

func (e *Refused) Error() string { return e.msg }

// Options are hl watch's flags.
type Options struct {
	URL string
	// Token authenticates the connection. It travels as a WebSocket subprotocol, the one place
	// a browser can put it too, so the platform reads every client's token the same way.
	Token              string
	Experiment         string
	PlatformExperiment string
	Agent              string
	Kinds              string
	// Decisions asks for a whole platform experiment's stream without the job transitions that
	// need no decision.
	Decisions bool
	// ShowSubscription prints the first frame, the subscription the server serves.
	ShowSubscription bool
	Until            string
	// Follow streams until the timeout instead of returning with the first batch.
	Follow bool
	// NoWait returns once the stream has caught up.
	NoWait bool
	// Wake, when set, says of a change's kind, subject and value whether it ends the wait. Any
	// other change is neither printed nor passed by the cursor until one that does arrives, and
	// then goes out ahead of it: until then the next watch reads it again.
	Wake    func(kind, subject, value string) bool
	Timeout time.Duration
	// After is the change id to read on from instead of the saved one, 0 for none.
	After int64
	// CursorFile, when set, is where the id to read on from is kept between watches (cursorState).
	CursorFile string

	// Stdout/Stderr let callers (and tests) capture output instead of the process streams.
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes the watch and returns the process exit code: 0 once a batch of changes was printed
// (no --until), --until became true, the window elapsed (no --until) or the saved cursor was reset;
// 124 on timeout with --until still unmet; 2 on a usage error or a refused subscription; 1 when a
// watch without --until never connected, a change could not be written to Stdout, or the cursor
// could not be saved.
func Run(opts Options) int {
	if opts.URL == "" {
		fmt.Fprintln(opts.Stderr, "hl watch: no API URL given (set API_URL)")
		return UsageExitCode
	}
	if opts.Token == "" {
		fmt.Fprintln(opts.Stderr, "hl watch: no token given (set HL_TOKEN)")
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
	if opts.Wake != nil && (opts.Until != "" || opts.Follow || opts.NoWait) {
		fmt.Fprintln(opts.Stderr, "hl watch: --wake goes with none of --until, --follow and --no-wait")
		return UsageExitCode
	}

	w := &watch{opts: opts, after: opts.After}
	if opts.Until != "" {
		fn, err := parseUntil(opts.Until)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "hl watch: %s\n", err)
			return UsageExitCode
		}
		w.satisfied = fn
	}
	if opts.CursorFile != "" && opts.After == 0 {
		state, err := loadCursor(opts.CursorFile)
		if err != nil {
			fmt.Fprintf(opts.Stderr, "hl watch: %s\n", err)
			return UsageExitCode
		}
		w.after = state.After
	}

	code := w.run()
	if code != UsageExitCode && opts.CursorFile != "" && w.moved {
		if err := saveCursor(opts.CursorFile, w.after); err != nil {
			fmt.Fprintf(opts.Stderr, "hl watch: %s\n", err)
			return 1
		}
	}
	return code
}

// watch is one hl watch invocation: the id it reads on from, and when it stops.
type watch struct {
	opts      Options
	satisfied func(event map[string]any) bool
	after     int64
	// moved says after moved past where the watch started, so it is saved.
	moved bool
	// reset is the after the platform no longer holds the changes after, 0 while it holds them:
	// the watch reads on from the head and says so.
	reset    int64
	printed  bool
	settleBy time.Time
	writeErr error
	// held are the changes a watch with Wake has read and not printed, in the order they came.
	held []heldChange
}

type heldChange struct {
	line string
	id   int64
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
		query.Set("agent_id", w.opts.Agent)
	}
	if w.opts.Decisions {
		query.Set("decisions", "true")
	}
	if w.opts.Kinds != "" {
		query.Set("kinds", w.opts.Kinds)
	}

	deadline := time.Now().Add(w.opts.Timeout)
	connected := false
	var connectErr error
	for time.Now().Before(w.until(deadline)) {
		switch {
		case w.reset != 0:
			query.Set("after", "head")
		case w.after != 0:
			query.Set("after", strconv.FormatInt(w.after, 10))
		default:
			query.Del("after")
		}
		ws, err := connect(w.opts.URL, "/watch?"+query.Encode(), w.opts.Token, time.Until(deadline))
		if err != nil {
			var refused *Refused
			if errors.As(err, &refused) && refused.reason == platformapi.ReasonCursorGone && w.opts.CursorFile != "" && w.opts.After == 0 && w.reset == 0 {
				w.reset = w.after
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
			fmt.Fprintf(w.opts.Stderr, "hl watch: write change: %s\n", w.writeErr)
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
	for time.Now().Before(w.until(deadline)) {
		line, err := ws.nextText(w.until(deadline))
		if err != nil {
			if !time.Now().Before(w.until(deadline)) {
				return !w.settleBy.IsZero()
			}
			fmt.Fprintf(w.opts.Stderr, "hl watch: stream ended (%s), reconnecting after change %d\n", err, w.after)
			sleepUntil(w.until(deadline), reconnectDelay)
			return false
		}
		var f platformapi.Frame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			fmt.Fprintf(w.opts.Stderr, "hl watch: a frame that is no JSON object: %s\n", line)
			continue
		}
		switch {
		case f.Subscription != nil:
			if w.opts.ShowSubscription {
				if w.writeErr = w.write(line); w.writeErr != nil {
					return true
				}
			}
		case f.Event != nil:
			if w.change(heldChange{line: line, id: f.ID}, *f.Event) {
				return true
			}
		case f.Progress != nil:
			if w.reset != 0 {
				return w.resetTo(f.Next)
			}
			if len(w.held) == 0 && f.Next > w.after {
				w.after, w.moved = f.Next, true
			}
			if f.CaughtUp && w.opts.NoWait {
				return true
			}
		}
	}
	return !w.settleBy.IsZero()
}

// change prints c, or holds it until one that wakes the watch arrives, and says whether the
// watch is done.
func (w *watch) change(c heldChange, f platformapi.Event) bool {
	if w.opts.Wake != nil && w.settleBy.IsZero() && !w.opts.Wake(f.Kind, f.Subject, f.Value) {
		// A reconnect reads again what is held, since the cursor has not passed it.
		if len(w.held) == 0 || c.id > w.held[len(w.held)-1].id {
			w.held = append(w.held, c)
		}
		return false
	}
	for _, h := range append(w.held, c) {
		if w.writeErr = w.print(h); w.writeErr != nil {
			return true
		}
	}
	w.held = nil
	if w.satisfied != nil {
		var event map[string]any
		_ = json.Unmarshal([]byte(c.line), &event)
		return w.satisfied(event)
	}
	return false
}

// resetTo reads on from head, the newest change, after the platform refused the saved cursor, and
// says so in one line that wakes whoever waits.
func (w *watch) resetTo(head int64) bool {
	w.after, w.moved = head, true
	line, _ := json.Marshal(map[string]string{"kind": platformapi.ResetKind,
		"detail": fmt.Sprintf("changes after %d are no longer held: re-read state (hl situation)", w.reset)})
	w.writeErr = w.write(string(line))
	return true
}

func (w *watch) write(line string) error {
	_, err := fmt.Fprintln(w.opts.Stdout, line)
	return err
}

// print writes one change and moves the cursor past it. The cursor moves only once the change is
// written, so one that was not is read again.
func (w *watch) print(c heldChange) error {
	if err := w.write(c.line); err != nil {
		return err
	}
	w.after, w.moved, w.printed = c.id, true, true
	if w.satisfied == nil && !w.opts.Follow && w.settleBy.IsZero() {
		w.settleBy = time.Now().Add(settleWindow)
	}
	return nil
}

// cursorState is what the cursor file holds: the id of the change to read on from.
type cursorState struct {
	After int64 `json:"after"`
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

func saveCursor(path string, after int64) error {
	data, err := json.Marshal(cursorState{After: after})
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
// change (their values never overlap: jobs are upper case, runs lower case), the only form hl
// watch understands. A wait that can never end is the failure this tool exists to remove, so an
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
		return (kind == platformapi.EventExperimentStatus || kind == platformapi.EventPlatformExperimentStatus) && wanted[value]
	}, nil
}

// --- minimal RFC 6455 client -----------------------------------------------------------------

type webSocket struct {
	conn net.Conn
	buf  []byte
}

func connect(base, path, token string, budget time.Duration) (*webSocket, error) {
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
	// The handshake, the upgrade request and its answer all fall within the dial's budget.
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
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
		"Sec-WebSocket-Protocol: " + platformapi.WatchSubprotocol + ", " + token + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}

	ws := &webSocket{conn: conn}
	head, err := ws.readUntilHeaderEnd()
	if err != nil {
		conn.Close()
		return nil, err
	}
	statusLine := strings.SplitN(string(head), "\r\n", 2)[0]
	if strings.Contains(statusLine, " 101 ") {
		// A proxy between here and the platform checks this too, and refuses the upgrade when
		// it is wrong: checking it here is what makes a platform that answers wrongly fail in
		// its own tests and not first behind a proxy.
		if got, want := headerValue(string(head), "Sec-WebSocket-Accept"), acceptFor(key); got != want {
			conn.Close()
			return nil, &Closed{msg: fmt.Sprintf("handshake failed: the platform answered the upgrade with Sec-WebSocket-Accept %q, and %q is the answer to the key sent", got, want)}
		}
		return ws, nil
	}
	code := statusCode(statusLine)
	reason, detail := ws.readRefusalBody(head)
	conn.Close()
	msg := fmt.Sprintf("the platform refused this subscription: %s%s", strings.TrimSpace(statusLine), detail)
	if code >= 400 && code < 500 {
		return nil, &Refused{msg: msg, reason: reason}
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

// readRefusalBody returns the reason of a refusal's error body and " — <message>", which is the
// only thing that says which part of the request was wrong.
func (ws *webSocket) readRefusalBody(head []byte) (reason, detail string) {
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
	var payload platformapi.ErrorModel
	message := text
	if json.Unmarshal(body, &payload) == nil && payload.Message != "" {
		message = payload.Message
	}
	if message == "" {
		return payload.Reason, ""
	}
	return payload.Reason, " — " + message
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
	_ = ws.conn.SetWriteDeadline(time.Now().Add(readTimeout))
	_, err := ws.conn.Write(append(append(header, mask...), masked...))
	return err
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
			// A close frame's text, after its two-byte code, is the reason the stream ended for.
			if len(payload) > 2 {
				return "", &Closed{msg: "server closed the stream: " + string(payload[2:])}
			}
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

// acceptFor is the answer RFC 6455 gives to a handshake key.
func acceptFor(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // the accept token is specified as SHA-1
	return base64.StdEncoding.EncodeToString(sum[:])
}

func headerValue(head, name string) string {
	for _, line := range strings.Split(head, "\r\n")[1:] {
		if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
