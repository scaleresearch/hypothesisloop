package fly

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestLogsKeepsTheCommandsLinesAndDropsTheVendorsOwn(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, logsPath, answer(200, logJSON(
		"Pulling container image registry.example/agent:1",
		"\x1b[32m INFO\x1b[0m Starting init (commit: 6172d83)...",
		"\x1b[32m INFO\x1b[0m Preparing to run: `/bin/sh -c hl-out|not output` as root",
		"hl-out|first",
		"2026/10/05 00:00:47 INFO SSH listening listen_address=[fdaa::2]:22",
		"hl-out|",
		"hl-out|  two  spaces \\ and a | pipe",
		"hl-out|hl-out|a line that starts like the prefix",
		"\x1b[32m INFO\x1b[0m Sending signal SIGTERM to main child process w/ PID 637",
		"hl-out|last, without a newline",
		"hl-ended|3",
		shutdownLine,
		"[    6.975632] reboot: Restarting system",
		policyLine,
	)))
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "9001", 100)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	want := []string{"first", "", `  two  spaces \ and a | pipe`, "hl-out|a line that starts like the prefix", "last, without a newline"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Logs\n got %q\nwant %q", got, want)
	}
	reads := api.sent(http.MethodGet, logsPath)
	if len(reads) != 1 || reads[0].Query != "instance=9001&limit=150" || reads[0].Auth != testToken {
		t.Errorf("reads = %+v, want one of the machine's last 150 entries", reads)
	}
}

func TestLogsReturnsTheLastTailLines(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, logsPath, answer(200, logJSON("hl-out|1", "hl-out|2", shutdownLine, "hl-out|3", "hl-ended|0")))
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "9001", 2)
	if err != nil || !reflect.DeepEqual(got, []string{"2", "3"}) {
		t.Fatalf("Logs = %q, %v, want the last two lines, oldest first", got, err)
	}
}

// chatty answers a log read as the vendor does, with the last limit entries of a log in which
// every third entry is the command's.
func chatty(total int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var messages []string
		for i := max(0, total-limit); i < total; i++ {
			if i%3 == 0 {
				messages = append(messages, fmt.Sprintf("hl-out|line %d", i))
			} else {
				messages = append(messages, "[    6.975632] kernel: noise")
			}
		}
		answer(200, logJSON(messages...))(w, r)
	}
}

func TestLogsReadsFurtherBackWhenTheVendorsLinesCrowdTheCommandsOut(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, logsPath, chatty(3000))
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "9001", 100)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(got) != 100 || got[0] != "line 2700" || got[99] != "line 2997" {
		t.Fatalf("Logs = %d lines from %q to %q, want the last 100 the command wrote", len(got), got[0], got[len(got)-1])
	}
	var asked []string
	for _, read := range api.sent(http.MethodGet, logsPath) {
		asked = append(asked, strings.TrimPrefix(read.Query, "instance=9001&limit="))
	}
	if !reflect.DeepEqual(asked, []string{"150", "300"}) {
		t.Errorf("reads asked for %v entries, want 150 and then 300", asked)
	}
}

func TestLogsOfAShortLogStopsAtItsStart(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, logsPath, chatty(30))
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "9001", 100)
	if err != nil || len(got) != 10 {
		t.Fatalf("Logs = %d lines, %v, want the 10 the command wrote", len(got), err)
	}
	if api.total() != 1 {
		t.Errorf("reads = %d, want 1: the first came back short, so it held the whole log", api.total())
	}
}

func TestLogsOfSomethingThatNeverWroteIsEmpty(t *testing.T) {
	for name, body := range map[string]string{
		"a machine that never started": logJSON("Pulling container image registry.example/agent:1"),
		"an id the app never had":      `{"data":[],"meta":{"next_token":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, logsPath, answer(200, body))
			p := newTestProvider(t, api)

			got, err := p.Logs(context.Background(), "9001", 10)
			if err != nil || got == nil || len(got) != 0 {
				t.Fatalf("Logs = %#v, %v, want an empty list", got, err)
			}
		})
	}
}

func TestLogsRefusesATailThatIsNotPositive(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api)

	for _, tail := range []int{0, -1} {
		if _, err := p.Logs(context.Background(), "9001", tail); err == nil || !strings.Contains(err.Error(), "tail must be positive") {
			t.Errorf("Logs(tail %d) = %v, want a refusal", tail, err)
		}
	}
	if api.total() != 0 {
		t.Errorf("requests = %d, want none", api.total())
	}
}

func TestLogsReportsEveryFailure(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"rate limited":     answer(429, `rate limited`),
		"server error":     answer(500, `oops`),
		"token not for it": answer(401, ``),
		"not a log":        answer(200, `<html>`),
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, logsPath, handler)
			p := newTestProvider(t, api)

			if got, err := p.Logs(context.Background(), "9001", 10); err == nil {
				t.Fatalf("Logs = %q with no error", got)
			}
			if api.total() != 1 {
				t.Errorf("requests = %d, want 1 and no retry", api.total())
			}
		})
	}
}
