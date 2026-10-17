package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/cpclient"
)

// answers serves one status per request, in order, and records what it was sent.
func answers(t *testing.T, statuses ...int) (*httptest.Server, *[]string) {
	t.Helper()
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = append(received, r.Header.Get("Authorization")+" "+string(body))
		if len(received) > len(statuses) {
			t.Errorf("request %d: only %d were expected", len(received), len(statuses))
			w.WriteHeader(http.StatusTeapot)
			return
		}
		status := statuses[len(received)-1]
		w.WriteHeader(status)
		if status/100 != 2 {
			_, _ = io.WriteString(w, `{"message":"unauthenticated: cluster edge-7 is not registered"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server, &received
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &out
}

func TestARefusedObservationIsLoggedWithItsReason(t *testing.T) {
	logged := captureLog(t)
	server, received := answers(t, http.StatusUnauthorized)
	observation := clusterapi.NodeMetricsPush{Node: "n", Timestamp: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), Pods: []clusterapi.PodSample{{PodUID: "p", ExperimentID: "job"}}}
	push(t.Context(), &cpclient.Client{BaseURL: server.URL, Token: "tok", HTTP: server.Client()}, observation)
	if len(*received) != 1 || !strings.HasPrefix((*received)[0], "Bearer tok ") {
		t.Fatalf("requests = %q, want one carrying the runtime token", *received)
	}
	if !strings.Contains(logged.String(), "status 401") || !strings.Contains(logged.String(), "cluster edge-7 is not registered") {
		t.Errorf("log = %q, want the status and the control plane's reason", logged.String())
	}
}
