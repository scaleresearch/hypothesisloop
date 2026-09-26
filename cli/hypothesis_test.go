package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

type recorded struct {
	Method, Path, Query string
	Body                map[string]any
}

// recordingServer answers every request with reply(n) for the n-th request and records it.
func recordingServer(t *testing.T, reply func(n int, r recorded) any) *[]recorded {
	t.Helper()
	var got []recorded
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.Body); err != nil {
				t.Errorf("request body is not a JSON object: %s", raw)
			}
		}
		got = append(got, rec)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply(len(got)-1, rec))
	})
	setAPIURL(t, url)
	return &got
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/hypotheses.yaml"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func links(pairs ...string) []any {
	out := []any{}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, map[string]any{"type": pairs[i], "to": pairs[i+1]})
	}
	return out
}

func lines(s string) []string {
	return strings.Split(strings.TrimSpace(s), "\n")
}

func TestHypothesisSubmitFlagsBuildLinks(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{"id": "hyp-9", "already_existed": false}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "submit", "--agent", "jane", "--platform-experiment", "pe-1", "--text", "warmup helps",
		"--builds-on", "root", "--builds-on", "hyp-1", "--contradicts", "hyp-2", "--related", "hyp-3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if len(*got) != 1 || (*got)[0].Method != "POST" || (*got)[0].Path != "/hypotheses" {
		t.Fatalf("requests: %+v", *got)
	}
	want := map[string]any{
		"agent_id": "jane", "platform_experiment_id": "pe-1", "text": "warmup helps",
		"links": links("builds_on", "pe-1", "builds_on", "hyp-1", "contradicts", "hyp-2", "related", "hyp-3"),
	}
	if !reflect.DeepEqual((*got)[0].Body, want) {
		t.Fatalf("body = %v\nwant  %v", (*got)[0].Body, want)
	}
	if stdout.String() != `{"already_existed":false,"id":"hyp-9"}`+"\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

// A dedup hit applies none of the caller's links, which the agent must hear about to place its idea.
func TestHypothesisSubmitDedupHitSaysTheLinksWereNotApplied(t *testing.T) {
	recordingServer(t, func(int, recorded) any {
		return map[string]any{"id": "hyp-1", "agent_id": "bob", "already_existed": true, "links": links("builds_on", "pe-1")}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "submit", "--agent", "jane", "--platform-experiment", "pe-1", "--text", "warmup helps",
		"--builds-on", "hyp-7"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	for _, want := range []string{"hyp-1", "bob", "links were not applied", "--builds-on hyp-1"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not say %q: %s", want, stderr.String())
		}
	}
}

func TestHypothesisSubmitWithoutBuildsOnIsUsageError(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{} })
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "submit", "--agent", "jane", "--platform-experiment", "pe-1", "--text", "t", "--related", "hyp-3"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if len(*got) != 0 {
		t.Fatalf("sent %d requests for an unplaced hypothesis", len(*got))
	}
	for _, want := range []string{"--builds-on root", "--builds-on ID", "hl hypothesis query"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr does not tell the agent %q: %s", want, stderr.String())
		}
	}
}

func TestHypothesisSubmitRootOnlyForBuildsOn(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{} })
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "submit", "--agent", "jane", "--platform-experiment", "pe-1", "--text", "t",
		"--builds-on", "root", "--contradicts", "root"}, &stdout, &stderr)
	if code != 2 || len(*got) != 0 || !strings.Contains(stderr.String(), "only builds_on may target root") {
		t.Fatalf("exit %d, requests %d, stderr %s", code, len(*got), stderr.String())
	}
}

func TestHypothesisSubmitBatchResolvesLocalKeys(t *testing.T) {
	got := recordingServer(t, func(n int, r recorded) any {
		return map[string]any{"id": fmt.Sprintf("hyp-%d", n), "text": r.Body["text"]}
	})
	file := writeFile(t, `agent_id: jane
platform_experiment_id: pe-1
links: [{type: builds_on, to: root}]
hypotheses:
  - key: lr
    text: higher lr
  - key: warm
    text: warmup
  - text: higher lr with warmup
    agent_id: bob
    links:
      - {type: builds_on, to: lr}
      - {type: builds_on, to: warm}
      - {type: contradicts, to: hyp-external}
`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "submit", "--file", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if len(*got) != 3 {
		t.Fatalf("want 3 POSTs, got %+v", *got)
	}
	for i, want := range []map[string]any{
		{"agent_id": "jane", "platform_experiment_id": "pe-1", "text": "higher lr", "links": links("builds_on", "pe-1")},
		{"agent_id": "jane", "platform_experiment_id": "pe-1", "text": "warmup", "links": links("builds_on", "pe-1")},
		{"agent_id": "bob", "platform_experiment_id": "pe-1", "text": "higher lr with warmup",
			"links": links("builds_on", "hyp-0", "builds_on", "hyp-1", "contradicts", "hyp-external")},
	} {
		if !reflect.DeepEqual((*got)[i].Body, want) {
			t.Fatalf("item %d body = %v\nwant %v", i, (*got)[i].Body, want)
		}
	}
	out := lines(stdout.String())
	if len(out) != 3 || !strings.Contains(out[2], `"id":"hyp-2"`) {
		t.Fatalf("want one JSON object per item, got %q", stdout.String())
	}
}

func TestHypothesisSubmitBatchFailsBeforeSendingAnything(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"forward key": {`hypotheses:
  - text: child
    links: [{type: builds_on, to: parent}]
  - key: parent
    text: parent
    links: [{type: builds_on, to: root}]
`, "item 0: link to key \"parent\", which item 1 defines"},
		"duplicate key": {`links: [{type: builds_on, to: root}]
hypotheses:
  - {key: a, text: one}
  - {key: a, text: two}
`, "item 1: key \"a\" already used by item 0"},
		"missing text": {`links: [{type: builds_on, to: root}]
hypotheses:
  - text: idea one
  - agent_id: bob
`, "item 1: agent_id (or author), platform_experiment_id and text are required"},
		"unplaced item": {`hypotheses:
  - text: placed
    links: [{type: builds_on, to: root}]
  - text: unplaced
`, "item 1: at least one --builds-on"},
		"bad link type": {`hypotheses:
  - text: t
    links: [{type: extends, to: root}]
`, `link type "extends" must be one of builds_on, contradicts, related`},
		"unknown link field": {`hypotheses:
  - text: t
    links: [{type: builds_on, target: root}]
`, `links[0]: unknown field "target"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "x"} })
			file := writeFile(t, "agent_id: jane\nplatform_experiment_id: pe-1\n"+tc.yaml)
			var stdout, stderr bytes.Buffer
			code := run([]string{"hypothesis", "submit", "--file", file}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit %d, want 2; stderr %s", code, stderr.String())
			}
			if len(*got) != 0 {
				t.Fatalf("sent %d requests before rejecting the file", len(*got))
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %s, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestHypothesisSubmitFlagLinksOverrideFileLinks(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "x"} })
	file := writeFile(t, `- agent_id: jane
  platform_experiment_id: pe-1
  text: one
  links: [{type: builds_on, to: hyp-old}]
- agent_id: jane
  platform_experiment_id: pe-2
  text: two
`)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "submit", "--file", file, "--builds-on", "root"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if len(*got) != 2 {
		t.Fatalf("want 2 POSTs, got %d", len(*got))
	}
	for i, pe := range []string{"pe-1", "pe-2"} {
		if !reflect.DeepEqual((*got)[i].Body["links"], links("builds_on", pe)) {
			t.Fatalf("item %d links = %v", i, (*got)[i].Body["links"])
		}
	}
}

func TestHypothesisSubmitBatchStopsAtFirstAPIError(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"unknown link target hyp-gone"}`))
	})
	setAPIURL(t, url)
	file := writeFile(t, `agent_id: jane
platform_experiment_id: pe-1
links: [{type: builds_on, to: hyp-gone}]
hypotheses: [{text: one}, {text: two}]
`)
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "submit", "--file", file}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "item 0") || !strings.Contains(stderr.String(), "hyp-gone") {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
}

func TestHypothesisSubmitHeldListsTheCloseOnesAndBothWaysOn(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":409,"detail":"Nothing was registered","similar":[` +
			`{"id":"hyp-1","agent_id":"bob","status":"open","text":"one","reason":"close_text"},` +
			`{"id":"hyp-2","author":"ann","status":"refuted","text":"two","reason":"refuted_parent"}]}`))
	})
	setAPIURL(t, url)
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "submit", "--agent", "jane", "--platform-experiment", "pe-1", "--text", "t", "--builds-on", "hyp-2"}, &stdout, &stderr)
	got := lines(stderr.String())
	if code != 1 || stdout.Len() != 0 || len(got) != 4 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if got[1] != `{"id":"hyp-1","agent_id":"bob","status":"open","text":"one","reason":"close_text"}` {
		t.Fatalf("held hypothesis line: %s", got[1])
	}
	for _, want := range []string{"hl job submit FILE --hypothesis ID", "--distinct-from hyp-1,hyp-2", "--contradicts ID"} {
		if !strings.Contains(got[3], want) {
			t.Fatalf("the ways on do not name %q: %s", want, got[3])
		}
	}
	if strings.Contains(stderr.String(), "Nothing was registered") {
		t.Fatalf("the reply body is printed as well: %s", stderr.String())
	}
}

func TestHypothesisList(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return []map[string]any{{"id": "hyp-1", "child_count": 2}, {"id": "hyp-2", "child_count": 0}}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "list", "--platform-experiment", "pe-1", "--agent", "jane", "--status", "open", "--limit", "5", "--offset", "10"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Path != "/hypotheses" || (*got)[0].Query != "agent=jane&limit=5&offset=10&platform_experiment_id=pe-1&status=open" {
		t.Fatalf("request = %+v", (*got)[0])
	}
	if want := "{\"child_count\":2,\"id\":\"hyp-1\"}\n{\"child_count\":0,\"id\":\"hyp-2\"}\n"; stdout.String() != want {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestHypothesisListRequiresPlatformExperiment(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "list"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}

func TestHypothesisGet(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{"id": "hyp-1", "links": map[string]any{"out": []any{}, "in": []any{}}}
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "get", "hyp-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Method != "GET" || (*got)[0].Path != "/hypotheses/hyp-1" {
		t.Fatalf("request = %+v", (*got)[0])
	}
	if stdout.String() != `{"id":"hyp-1","links":{"in":[],"out":[]},"jobs":[]}`+"\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if code := run([]string{"hypothesis", "get"}, &stdout, &stderr); code != 2 {
		t.Fatalf("get without ID: exit %d, want 2", code)
	}
}

func TestHypothesisLink(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return []map[string]any{{"type": "builds_on", "hypothesis_id": "hyp-2"}}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "link", "--agent", "jane", "--from", "hyp-1", "--type", "builds_on", "--to", "hyp-2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := map[string]any{"agent_id": "jane", "type": "builds_on", "to": "hyp-2"}
	if (*got)[0].Method != "POST" || (*got)[0].Path != "/hypotheses/hyp-1/links" || !reflect.DeepEqual((*got)[0].Body, want) {
		t.Fatalf("request = %+v", (*got)[0])
	}
	if stdout.String() != `[{"hypothesis_id":"hyp-2","type":"builds_on"}]`+"\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestHypothesisLinkUsageErrors(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return nil })
	for _, args := range [][]string{
		{"--agent", "jane", "--from", "hyp-1", "--type", "extends", "--to", "hyp-2"},
		{"--agent", "jane", "--from", "hyp-1", "--type", "related"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"hypothesis", "link"}, args...), &stdout, &stderr); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("usage errors sent %d requests", len(*got))
	}
}

func TestHypothesisQuery(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{
			"columns":   []string{"id", "text", "n"},
			"rows":      [][]any{{"hyp-1", "warmup", 3}, {"hyp-2", nil, map[string]any{"_labels": []string{"Hypothesis"}}}},
			"truncated": false,
		}
	})
	cypher := "MATCH (c:Hypothesis)-[:BUILDS_ON]->(:Hypothesis {id: $id}) RETURN c.id AS id, c.text AS text, 3 AS n"
	var stdout, stderr bytes.Buffer
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
	code := run([]string{"hypothesis", "query", cypher, "--param", "id=hyp-0", "--param", "q=a=b", "--platform-experiment", "pe-1"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := map[string]any{"platform_experiment_id": "pe-1", "cypher": cypher, "params": map[string]any{"id": "hyp-0", "q": "a=b"}}
	if (*got)[0].Method != "POST" || (*got)[0].Path != "/hypotheses/query" || !reflect.DeepEqual((*got)[0].Body, want) {
		t.Fatalf("request = %+v", (*got)[0])
	}
	wantOut := `{"id":"hyp-1","text":"warmup","n":3}` + "\n" + `{"id":"hyp-2","text":null,"n":{"_labels":["Hypothesis"]}}` + "\n"
	if stdout.String() != wantOut {
		t.Fatalf("stdout = %q\nwant     %q", stdout.String(), wantOut)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
}

func TestHypothesisQueryTruncatedWarnsOnStderr(t *testing.T) {
	recordingServer(t, func(int, recorded) any {
		return map[string]any{"columns": []string{"id"}, "rows": [][]any{{"a"}, {"b"}}, "truncated": true}
	})
	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "query", "MATCH (h:Hypothesis) RETURN h.id AS id"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if len(lines(stdout.String())) != 2 || !strings.Contains(stderr.String(), "truncated at 2 rows") {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestHypothesisQueryFromStdin(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any {
		return map[string]any{"columns": []string{}, "rows": [][]any{}, "truncated": false}
	})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	_, _ = w.WriteString("MATCH (n) RETURN count(n)\n")
	w.Close()

	t.Setenv("PLATFORM_EXPERIMENT_ID", "pe-env")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "query", "--param", "x=1", "-"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if (*got)[0].Body["cypher"] != "MATCH (n) RETURN count(n)\n" || (*got)[0].Body["platform_experiment_id"] != "pe-env" {
		t.Fatalf("body = %v", (*got)[0].Body)
	}
}

func TestHypothesisQueryUsageErrors(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return nil })
	t.Setenv("PLATFORM_EXPERIMENT_ID", "")
	for _, args := range [][]string{
		{},
		{"MATCH (n) RETURN n"},
		{"MATCH (n) RETURN n", "--platform-experiment", "pe-1", "extra"},
		{"MATCH (n) RETURN n", "--platform-experiment", "pe-1", "--param", "novalue"},
		{"   ", "--platform-experiment", "pe-1"},
		{`{"cypher":"MATCH (n) RETURN n"}`, "--platform-experiment", "pe-1"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"hypothesis", "query"}, args...), &stdout, &stderr); code != 2 {
			t.Fatalf("%q: exit %d, want 2", args, code)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("usage errors sent %d requests", len(*got))
	}
}

func TestHypothesisQueryErrorSurfacesServerMessage(t *testing.T) {
	url := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"line 1:7 mismatched input 'MATC'"}`))
	})
	setAPIURL(t, url)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "query", "MATC (n) RETURN n", "--platform-experiment", "pe-1"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "mismatched input") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestHypothesisExampleFileSubmits(t *testing.T) {
	got := recordingServer(t, func(n int, r recorded) any { return map[string]any{"id": fmt.Sprintf("hyp-%d", n)} })
	var stdout, stderr bytes.Buffer
	if code := run([]string{"hypothesis", "submit", "--file", "../controlplane/settings/examples/hypothesis.yaml"}, &stdout, &stderr); code != 0 {
		t.Fatalf("the DSL reference no longer submits: exit %d, stderr %s", code, stderr.String())
	}
	if len(*got) != 3 || !reflect.DeepEqual((*got)[2].Body["links"], links("builds_on", "hyp-0", "builds_on", "hyp-1")) {
		t.Fatalf("requests = %+v", *got)
	}
}

func TestHypothesisComment(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "c-1"} })
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "comment", "hyp-1", "--agent", "jane", "--text", "dead end"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := map[string]any{"agent_id": "jane", "text": "dead end"}
	if (*got)[0].Method != "POST" || (*got)[0].Path != "/hypotheses/hyp-1/comments" || !reflect.DeepEqual((*got)[0].Body, want) {
		t.Fatalf("request = %+v", (*got)[0])
	}
}

func TestHypothesisStatus(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "hyp-1"} })
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
	got := recordingServer(t, func(int, recorded) any { return nil })
	for _, args := range [][]string{
		{"hypothesis", "comment", "--agent", "jane", "--text", "x"},
		{"hypothesis", "comment", "hyp-1", "--text", "x"},
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

func TestHypothesisSubmitSendsDistinctFrom(t *testing.T) {
	got := recordingServer(t, func(n int, r recorded) any { return map[string]any{"id": fmt.Sprintf("hyp-%d", n)} })
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "submit", "--agent", "jane", "--platform-experiment", "pe-1", "--text", "lr 0.2 wins",
		"--builds-on", "hyp-1", "--distinct-from", "hyp-1,hyp-3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if want := []any{"hyp-1", "hyp-3"}; !reflect.DeepEqual((*got)[0].Body["distinct_from"], want) {
		t.Fatalf("distinct_from = %v, want %v", (*got)[0].Body["distinct_from"], want)
	}

	file := writeFile(t, `agent_id: jane
platform_experiment_id: pe-1
hypotheses:
  - key: lr
    text: higher lr
    links: [{type: builds_on, to: root}]
  - text: higher lr with warmup
    links: [{type: builds_on, to: lr}]
    distinct_from: [lr, hyp-external]
`)
	if code := run([]string{"hypothesis", "submit", "--file", file}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if _, set := (*got)[1].Body["distinct_from"]; set {
		t.Fatalf("item without distinct_from sent one: %v", (*got)[1].Body)
	}
	if want := []any{"hyp-1", "hyp-external"}; !reflect.DeepEqual((*got)[2].Body["distinct_from"], want) {
		t.Fatalf("file distinct_from = %v, want %v (local key resolved)", (*got)[2].Body["distinct_from"], want)
	}
}

func TestHypothesisSubmitTakesCommaSeparatedIDs(t *testing.T) {
	got := recordingServer(t, func(int, recorded) any { return map[string]any{"id": "hyp-9"} })
	var stdout, stderr bytes.Buffer
	code := run([]string{"hypothesis", "submit", "--agent", "jane", "--platform-experiment", "pe-1", "--text", "combo",
		"--builds-on", "hyp-1,hyp-2", "--related", "hyp-3"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := links("builds_on", "hyp-1", "builds_on", "hyp-2", "related", "hyp-3")
	if !reflect.DeepEqual((*got)[0].Body["links"], want) {
		t.Fatalf("links = %v, want %v", (*got)[0].Body["links"], want)
	}
}

// Every job of hl hypothesis get read "env":{} in a run where each hypothesis had one job: env is
// what differs between the listed jobs, and one job differs from nothing.
func TestTheOnlyJobOfAHypothesisShowsItsWholeEnvAndSeveralShowWhatDiffers(t *testing.T) {
	job := func(id, lr string) map[string]any {
		return map[string]any{"id": id, "status": "COMPLETED", "job": map[string]any{"env": map[string]any{"LR": lr, "HIDDEN": "16"}}}
	}
	var jobs []any
	recordingServer(t, func(int, recorded) any {
		return map[string]any{"id": "h-1", "text": "t", "job_count": len(jobs), "jobs": jobs}
	})
	get := func() string {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"hypothesis", "get", "h-1"}, &stdout, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		return stdout.String()
	}
	jobs = []any{job("job-1", "0.3")}
	if out := get(); !strings.Contains(out, `"env":{"HIDDEN":"16","LR":"0.3"}`) {
		t.Errorf("one job: %s, want its whole env", out)
	}
	jobs = []any{job("job-1", "0.3"), job("job-2", "0.1")}
	if out := get(); !strings.Contains(out, `"env":{"LR":"0.3"}`) || !strings.Contains(out, `"env":{"LR":"0.1"}`) || strings.Contains(out, "HIDDEN") {
		t.Errorf("two jobs: %s, want only what differs", out)
	}
}
