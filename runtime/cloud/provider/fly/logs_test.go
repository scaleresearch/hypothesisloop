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
		"hl-ended",
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
	if len(reads) != 1 || reads[0].Query != "instance=9001&limit=500" || reads[0].Auth != testToken {
		t.Errorf("reads = %+v, want one of the machine's last 500 entries", reads)
	}
}

func TestLogsReturnsTheLastTailLines(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, logsPath, answer(200, logJSON("hl-out|1", "hl-out|2", shutdownLine, "hl-out|3", "hl-ended")))
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "9001", 2)
	if err != nil || !reflect.DeepEqual(got, []string{"2", "3"}) {
		t.Fatalf("Logs = %q, %v, want the last two lines, oldest first", got, err)
	}
}

// A Machine started a second time boots the image again and runs its command again into the log
// of the first run. What a caller reads is the run that is the Machine's now: an exit line or a
// progress marker of the earlier one says nothing of it.
func TestLogsOfAMachineStartedAgainAreThoseOfItsNewestStart(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, logsPath, answer(200, logJSON(
		"hl-started", "hl-out|first run", "hl-out|hl-exit 0", "hl-ended", shutdownLine,
		"\x1b[32m INFO\x1b[0m Starting init (commit: 6172d83)...",
		"hl-started", "hl-out|second run", "hl-out|hl-started",
	)))
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "9001", 100)
	if err != nil || !reflect.DeepEqual(got, []string{"second run", "hl-started"}) {
		t.Fatalf("Logs = %q, %v, want the lines of the second start alone, a line of the command that reads like the mark among them", got, err)
	}
}

func TestLogsOfARunThatStartsInsideAFullReadAreTheWholeRun(t *testing.T) {
	api := newFakeAPI(t)
	entries := make([]string, logPage)
	for i := range entries {
		entries[i] = "[    6.975632] kernel: noise"
	}
	entries[logPage-3], entries[logPage-2], entries[logPage-1] = "hl-started", "hl-out|one", "hl-out|two"
	api.on(http.MethodGet, logsPath, answer(200, logJSON(entries...)))
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "9001", 100)
	if err != nil || !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("Logs = %q, %v, want both lines and no error: nothing of this run is further back", got, err)
	}
}

// chatty answers a log read as the vendor does, with the last entries of a log of total in which
// every third entry is the command's, and never more than a page of them.
func chatty(total int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var messages []string
		for i := max(0, total-min(limit, logPage)); i < total; i++ {
			if i%3 == 0 {
				messages = append(messages, fmt.Sprintf("hl-out|line %d", i))
			} else {
				messages = append(messages, "[    6.975632] kernel: noise")
			}
		}
		answer(200, logJSON(messages...))(w, r)
	}
}

func TestLogsFindsTheTailAmongTheVendorsLines(t *testing.T) {
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
	if api.total() != 1 {
		t.Errorf("reads = %d, want 1", api.total())
	}
}

func TestLogsRefusesATailOneReadCannotHold(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, logsPath, chatty(3000))
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "9001", 200)
	if err == nil || !strings.Contains(err.Error(), "hold 166 lines of the command, and 200 were asked for") {
		t.Fatalf("Logs = %d lines, %v, want an error: a short answer would pass for the whole output", len(got), err)
	}
	if api.total() != 1 {
		t.Errorf("reads = %d, want 1: the log cannot be read further back", api.total())
	}
}

func TestLogsOfAShortLogIsTheWholeLog(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, logsPath, chatty(30))
	p := newTestProvider(t, api)

	got, err := p.Logs(context.Background(), "9001", 100)
	if err != nil || len(got) != 10 {
		t.Fatalf("Logs = %d lines, %v, want the 10 the command wrote", len(got), err)
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
