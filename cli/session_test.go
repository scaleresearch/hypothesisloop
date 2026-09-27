package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestAgentsSessionWritesTheStoredBytesAsTheyAre(t *testing.T) {
	stored := []byte{0x1f, 0x8b, 0x00, 0xff, '\n', 0x00, 'x'}
	var asked string
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.Method + " " + r.URL.Path
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(stored)
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "agents", "session", "--platform-experiment", "pe-1", "ag-1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if asked != "GET /platform-experiments/pe-1/agents/ag-1/session" || !bytes.Equal(stdout.Bytes(), stored) {
		t.Fatalf("asked %q and wrote %v, want the stored bytes of the agent's session", asked, stdout.Bytes())
	}
}

func TestAgentsSessionSaysWhyItWasRefused(t *testing.T) {
	setAPIURL(t, withServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"reason":"not_found","message":"session not found"}`))
	}))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"platform-experiments", "agents", "session", "--platform-experiment", "pe-1", "ag-1"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "session not found") {
		t.Fatalf("stdout %q, stderr %q: want nothing written and the platform's reason", stdout.String(), stderr.String())
	}
}
