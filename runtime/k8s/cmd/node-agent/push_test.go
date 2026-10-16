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
			_, _ = io.WriteString(w, `{"error":"unauthenticated: cluster edge-7 is not registered"}`)
		}
	}))
	t.Cleanup(server.Close)
	return server, &received
}

func backlog(n int) []clusterapi.NodeMetricsPush {
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	out := make([]clusterapi.NodeMetricsPush, n)
	for i := range out {
		out[i] = clusterapi.NodeMetricsPush{Node: "n", Timestamp: start.Add(time.Duration(i) * time.Second), Pods: []clusterapi.PodSample{{PodUID: "p", ExperimentID: "job"}}}
	}
	return out
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&out)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &out
}

func TestARefusedObservationIsDroppedWithItsReasonAndTheRestStillGoes(t *testing.T) {
	logged := captureLog(t)
	server, received := answers(t, http.StatusUnauthorized, http.StatusBadRequest, http.StatusNoContent)
	left := flush(&cpclient.Client{BaseURL: server.URL, Token: "tok", HTTP: server.Client()}, backlog(3))
	if len(left) != 0 || len(*received) != 3 {
		t.Fatalf("left = %d after %d requests, want an empty backlog after 3", len(left), len(*received))
	}
	if !strings.HasPrefix((*received)[0], "Bearer tok ") {
		t.Errorf("request = %q, want the runtime token sent", (*received)[0])
	}
	if !strings.Contains(logged.String(), "status 401") || !strings.Contains(logged.String(), "cluster edge-7 is not registered") {
		t.Errorf("log = %q, want the status and the control plane's reason", logged.String())
	}
}

func TestAnObservationTheControlPlaneCouldNotTakeWaitsAtTheHeadOfTheQueue(t *testing.T) {
	logged := captureLog(t)
	server, received := answers(t, http.StatusNoContent, http.StatusServiceUnavailable)
	queue := backlog(3)
	left := flush(&cpclient.Client{BaseURL: server.URL, Token: "tok", HTTP: server.Client()}, queue)
	if len(left) != 2 || !left[0].Timestamp.Equal(queue[1].Timestamp) || len(*received) != 2 {
		t.Fatalf("left = %d after %d requests, want the second and third kept after 2", len(left), len(*received))
	}
	if !strings.Contains(logged.String(), "status 503") {
		t.Errorf("log = %q, want the status", logged.String())
	}

	server.Close()
	if left := flush(&cpclient.Client{BaseURL: server.URL, Token: "tok", HTTP: server.Client()}, queue); len(left) != 3 {
		t.Fatalf("with the control plane unreachable: left = %d, want all 3 kept", len(left))
	}
}
