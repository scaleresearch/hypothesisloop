package e2b

import (
	"context"
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func stdoutEvent(output string) string {
	return `{"event":{"data":{"stdout":"` + base64.StdEncoding.EncodeToString([]byte(output)) + `"}}}`
}

func stderrEvent(output string) string {
	return `{"event":{"data":{"stderr":"` + base64.StdEncoding.EncodeToString([]byte(output)) + `"}}}`
}

const endedEvent = `{"event":{"end":{"exited":true,"status":"exit status 0"}}}`

// tailing wires a Logs call whose tail writes the given chunks.
func tailing(api *fakeAPI, chunks ...string) {
	events := []string{startedEvent}
	for _, chunk := range chunks {
		events = append(events, stdoutEvent(chunk))
	}
	api.on(http.MethodGet, sandboxPath, answer(200, detailJSON))
	api.on(http.MethodPost, envdStart, stream(`{}`, append(events, endedEvent)...))
}

func TestLogsRunsTailOnTheOutputFileInsideTheSandbox(t *testing.T) {
	api := newFakeAPI(t)
	tailing(api, "first\n\n  two  spaces \\ kept\nlast, without a newline")
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "sb1", 200)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if want := []string{"first", "", `  two  spaces \ kept`, "last, without a newline"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Logs\n got %q\nwant %q", got, want)
	}
	starts := api.sent(http.MethodPost, envdStart)
	if len(starts) != 1 || starts[0].AccessToken != testToken {
		t.Fatalf("starts = %+v, want one with the sandbox's token", starts)
	}
	want := map[string]any{
		"process": map[string]any{"cmd": "/bin/sh", "args": []any{"-c", tailScript, "hl-logs", "/var/tmp/.hl-supervisor/out", "200"}},
		"stdin":   false,
	}
	if got := sentProcess(t, starts[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("process start\n got %v\nwant %v", got, want)
	}
}

func TestLogsJoinsOutputThatArrivesInPieces(t *testing.T) {
	api := newFakeAPI(t)
	tailing(api, "one\ntw", "o\nthr", "ee\n")
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "sb1", 10)
	if err != nil || !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("Logs = %q, %v", got, err)
	}
}

func TestLogsTellsNoOutputFromOneEmptyLine(t *testing.T) {
	for name, tc := range map[string]struct {
		chunks []string
		want   []string
	}{
		"the command never started or wrote nothing": {nil, []string{}},
		"the command wrote one empty line":           {[]string{"\n"}, []string{""}},
		"the last line is empty":                     {[]string{"a\n\n"}, []string{"a", ""}},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			tailing(api, tc.chunks...)
			p := newTestProvider(t, api)

			got, err := p.Logs(context.Background(), "sb1", 10)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Logs = %#v, %v, want %#v", got, err, tc.want)
			}
		})
	}
}

func TestLogsRefusesATailThatIsNotPositive(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)

	for _, tail := range []int{0, -1} {
		if _, err := p.Logs(context.Background(), "sb1", tail); err == nil || !strings.Contains(err.Error(), "tail must be positive") {
			t.Errorf("Logs(tail %d) = %v, want a refusal", tail, err)
		}
	}
	if api.total() != 0 {
		t.Errorf("requests = %d, want none", api.total())
	}
}

func TestLogsOfAGoneSandboxSaysItsOutputIsGone(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxPath, answer(404, goneJSON))
	p := newTestProvider(t, api)

	_, err := p.Logs(context.Background(), "sb1", 10)
	if err == nil || !strings.Contains(err.Error(), "the sandbox is gone") {
		t.Fatalf("err = %v, want one saying the sandbox is gone", err)
	}
}

func TestLogsReportsEveryFailure(t *testing.T) {
	detail := answer(200, detailJSON)
	cases := map[string]struct {
		detail, start http.HandlerFunc
		want          string
		requests      int
	}{
		"token rate limited": {detail: answer(429, `{"code":429}`), want: "429", requests: 1},
		"envd unreachable":   {detail: detail, start: answer(502, deadEnvd), want: "The sandbox was not found", requests: 2},
		"no tail in the template": {detail: detail, start: stream(`{}`, startedEvent, stderrEvent("/bin/sh: exec: line 1: tail: not found\n"),
			`{"event":{"end":{"exited":true,"status":"exit status 127"}}}`), want: "tail: not found", requests: 2},
		"tail killed":           {detail: detail, start: stream(`{}`, startedEvent, `{"event":{"end":{"exited":false,"status":"signal: killed"}}}`), want: "signal: killed", requests: 2},
		"the stream ends early": {detail: detail, start: stream(`{}`, startedEvent, stdoutEvent("half\n")), want: `ended with ""`, requests: 2},
		"the stream breaks off": {detail: detail, start: answer(200, string(framed(0, startedEvent))), want: "broke off", requests: 2},
		"the stream ends in an error": {detail: detail, start: stream(`{"error":{"code":"unavailable","message":"envd is shutting down"}}`, startedEvent),
			want: "envd is shutting down", requests: 2},
		"an event that is not JSON": {detail: detail, start: stream(`{}`, `<html>`), want: "decode a process event", requests: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, sandboxPath, tc.detail)
			if tc.start != nil {
				api.on(http.MethodPost, envdStart, tc.start)
			}
			p := newTestProvider(t, api)

			got, err := p.Logs(context.Background(), "sb1", 10)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Logs = %q, %v, want an error about %q", got, err, tc.want)
			}
			if api.total() != tc.requests {
				t.Errorf("requests = %d, want %d and no retry", api.total(), tc.requests)
			}
		})
	}
}
