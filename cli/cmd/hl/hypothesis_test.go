package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

// Agents read 20-25K characters from a list or a query, which stayed in their context for every
// later call. Texts are cut unless --full.
func TestHypothesisListAndQueryCutLongTextsUnlessFull(t *testing.T) {
	long := strings.Repeat("ab", 200)
	clitest.RecordingServer(t, func(_ int, r clitest.Recorded) any {
		if r.Path == "/hypotheses/query" {
			return map[string]any{"columns": []string{"id", "text"}, "rows": [][]any{{"hyp-1", long}}, "truncated": false}
		}
		return []map[string]any{{"id": "hyp-1", "text": long, "platform_experiment_id": "pe-1",
			"status_actor": map[string]any{"kind": "agent", "id": "a", "name": "a"}}}
	})
	cutText := long[:240] + "…"
	for _, tc := range []struct {
		args       []string
		want, note string
	}{
		{[]string{"hypothesis", "list", "--platform-experiment", "pe-1"},
			`{"id":"hyp-1","text":"` + cutText + `"}`, "1 texts cut to 240 characters"},
		{[]string{"hypothesis", "list", "--platform-experiment", "pe-1", "--full"},
			`{"id":"hyp-1","platform_experiment_id":"pe-1","status_actor":{"id":"a","kind":"agent","name":"a"},"text":"` + long + `"}`, ""},
		{[]string{"hypothesis", "query", "MATCH (h:Hypothesis) RETURN h.id AS id, h.text AS text", "--platform-experiment", "pe-1"},
			`{"id":"hyp-1","text":"` + cutText + `"}`, "1 values cut to 240 characters; --full"},
		{[]string{"hypothesis", "query", "MATCH (h:Hypothesis) RETURN h.id AS id, h.text AS text", "--platform-experiment", "pe-1", "--full"},
			`{"id":"hyp-1","text":"` + long + `"}`, ""},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d, stderr %s", tc.args, code, stderr.String())
		}
		if stdout.String() != tc.want+"\n" {
			t.Fatalf("%v: stdout = %q\nwant     %q", tc.args, stdout.String(), tc.want+"\n")
		}
		if got := strings.Contains(stderr.String(), "cut to"); got != (tc.note != "") || !strings.Contains(stderr.String(), tc.note) {
			t.Fatalf("%v: stderr = %q", tc.args, stderr.String())
		}
	}
}

func TestHypothesisStatus(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return map[string]any{"id": "hyp-1"} })
	var stdout, stderr bytes.Buffer
	want := map[string]any{"agent_id": "jane", "status": "refuted"}
	for i, args := range [][]string{
		{"hypothesis", "status", "--agent", "jane", "--status", "refuted", "hyp-1"},
		{"hypothesis", "status", "hyp-1", "refuted", "--agent", "jane"},
	} {
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d, stderr %s", args, code, stderr.String())
		}
		if (*got)[i].Method != "POST" || (*got)[i].Path != "/hypotheses/hyp-1/status" || !reflect.DeepEqual((*got)[i].Body, want) {
			t.Fatalf("request = %+v", (*got)[i])
		}
	}
}

func TestHypothesisCommentAndStatusUsageErrors(t *testing.T) {
	got := clitest.RecordingServer(t, func(int, clitest.Recorded) any { return nil })
	for _, args := range [][]string{
		{"hypothesis", "comment", "--agent", "jane", "--text", "x"},
		{"hypothesis", "comment", "hyp-1"},
		{"hypothesis", "status", "hyp-1", "--agent", "jane"},
		{"hypothesis", "status", "hyp-1", "hyp-2", "--agent", "jane", "--status", "open"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("usage errors sent requests: %+v", *got)
	}
}

func TestHelpGoesToStdout(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"hypothesis", "--help"}, {"job", "help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "usage: hl") {
			t.Fatalf("%v: exit %d, stdout %q", args, code, stdout.String())
		}
	}
}

func TestSubcommandHelpExitsZero(t *testing.T) {
	t.Setenv("API_URL", "http://example.invalid")
	t.Setenv("HL_TOKEN", "test-token")
	for _, args := range [][]string{
		{"job", "submit", "--help"}, {"job", "list", "-h"}, {"hypothesis", "query", "--help"},
		{"hypothesis", "get", "--help"}, {"platform-experiments", "update", "--help"}, {"watch", "--help"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 0 || !strings.Contains(strings.ToLower(stdout.String()+stderr.String()), "usage") {
			t.Fatalf("%v: exit %d, output %q", args, code, stdout.String()+stderr.String())
		}
	}
}
