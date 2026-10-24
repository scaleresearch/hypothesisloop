package runpod

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/vendorapi"
)

const (
	// logQuietFor is how long a log stream may send nothing before its backfill is taken as
	// complete. The stream stays open after the backfill and marks no end of it; the backfill
	// arrives at once.
	logQuietFor = 3 * time.Second
	// maxLogTail is the most lines of backfill the API sends.
	maxLogTail = 5000
	// maxLogLine bounds one event of the stream.
	maxLogLine = 1 << 20
)

type logEvent struct {
	line string
	at   time.Time
	err  error
}

// Logs reads the container's output from the API's log stream: the last tail lines as backfill,
// up to the first line written after the stream was opened or until the stream goes quiet.
func (p *Provider) Logs(ctx context.Context, id string, tail int) ([]string, error) {
	if tail <= 0 {
		return nil, fmt.Errorf("runpod: logs of pod %s: tail must be positive, got %d", id, tail)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	query := url.Values{"source": {"container"}, "tail": {strconv.Itoa(min(tail, maxLogTail))}}
	path := "/v2/pods/" + url.PathEscape(id) + "/logs"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("runpod: build logs request: %w", err)
	}
	p.authorize(req)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := p.logs.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runpod: logs of pod %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, refusal(&vendorapi.StatusError{Vendor: vendor, Method: http.MethodGet, Path: path, Status: resp.StatusCode, Header: resp.Header, Body: string(raw)})
	}
	opened, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return nil, fmt.Errorf("runpod: logs of pod %s: the answer carries no usable Date: %w", id, err)
	}
	events := make(chan logEvent)
	go readEvents(ctx, resp.Body, events)
	lines := []string{}
	quiet := time.NewTimer(p.logQuiet)
	defer quiet.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("runpod: logs of pod %s: %w", id, ctx.Err())
		case <-quiet.C:
			return last(lines, tail), nil
		case e, open := <-events:
			switch {
			case !open:
				return last(lines, tail), nil
			case e.err != nil:
				return nil, fmt.Errorf("runpod: logs of pod %s: %w", id, e.err)
			case e.at.After(opened.Add(time.Second)):
				return last(lines, tail), nil
			}
			lines = append(lines, e.line)
			quiet.Reset(p.logQuiet)
		}
	}
}

// readEvents sends the data of every event of the stream, and closes events at its end.
func readEvents(ctx context.Context, body io.Reader, events chan<- logEvent) {
	defer close(events)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLogLine)
	send := func(e logEvent) bool {
		select {
		case events <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for scanner.Scan() {
		data, isData := strings.CutPrefix(scanner.Text(), "data:")
		if !isData {
			continue
		}
		var event struct {
			Line string `json:"line"`
			TS   string `json:"ts"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &event); err != nil {
			send(logEvent{err: fmt.Errorf("an event is not the JSON the API documents: %w", err)})
			return
		}
		at, err := time.Parse(time.RFC3339Nano, event.TS)
		if err != nil {
			send(logEvent{err: fmt.Errorf("an event has no usable ts: %w", err)})
			return
		}
		if !send(logEvent{line: event.Line, at: at}) {
			return
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		send(logEvent{err: err})
	}
}

func last(lines []string, tail int) []string {
	return lines[max(0, len(lines)-tail):]
}
