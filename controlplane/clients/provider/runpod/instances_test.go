package runpod

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

func TestANameReadsBackWithItsMarks(t *testing.T) {
	for podName, want := range map[string]marked{
		"hl-job-a":             {name: "hl-job-a"},
		"hl-job-a.up":          {name: "hl-job-a", up: upMark},
		"hl-job-a.exited":      {name: "hl-job-a", up: exitMark},
		"hl-job-a.stop":        {name: "hl-job-a", stopped: true},
		"hl-job-a.stop.up":     {name: "hl-job-a", stopped: true, up: upMark},
		"hl-job-a.stop.exited": {name: "hl-job-a", stopped: true, up: exitMark},
	} {
		if got := parseName(podName); got != want {
			t.Errorf("parseName(%q) = %+v, want %+v", podName, got, want)
		}
		if got := parseName(podName).String(); got != podName {
			t.Errorf("%q reads back as %q", podName, got)
		}
	}
}

func TestListShowsThePrefixsPodsUnderTheirNamesInTheStateTheirMarksSay(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	first := `{"pagination":{"hasNextPage":true,"nextCursor":"c2"},"pods":[` + podJSON("p1", "hl-job-a", "RUNNING") + "," + podJSON("p2", "hl-job-b.up", "RUNNING") + "," + podJSON("p3", "other", "RUNNING") + `]}`
	api.on(http.MethodGet, "/v2/pods", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "c2" {
			reply(200, podsPage(podJSON("p4", "hl-job-c.stop.exited", "RUNNING"), podJSON("p5", "hl-job-d.up", "EXITED"), podJSON("p6", "hl-job-e", "TERMINATED"), podJSON("p7", "hl-job-f", "PROVISIONING")))(w, r)
			return
		}
		reply(200, first)(w, r)
	})
	listed, err := p.List(context.Background(), "hl-job-")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]provider.State{"p1": provider.StateStarting, "p2": provider.StateRunning, "p4": provider.StateExited, "p5": provider.StateUnknown, "p7": provider.StateStarting}
	if len(listed) != len(want) {
		t.Fatalf("listed %+v, want %d pods", listed, len(want))
	}
	for _, i := range listed {
		if i.State != want[i.ID] {
			t.Errorf("pod %s is %q, want %q", i.ID, i.State, want[i.ID])
		}
		if wantName := map[string]string{"p1": "hl-job-a", "p2": "hl-job-b", "p4": "hl-job-c", "p5": "hl-job-d", "p7": "hl-job-f"}[i.ID]; i.Name != wantName {
			t.Errorf("pod %s is named %q, want %q", i.ID, i.Name, wantName)
		}
		if i.CPUModel != "cpu3c" || i.PricePerHour != 0.06 || i.Architecture != "amd64" || i.StartedAt.IsZero() || i.Host != "" {
			t.Errorf("pod %s: %+v", i.ID, i)
		}
	}
	if q := api.sent(http.MethodGet, "/v2/pods"); len(q) != 2 || q[0].Auth != "Bearer "+testKey {
		t.Fatalf("requests %+v", q)
	}
}

func TestGetAndDestroyOfAPodThatIsGoneAreNoError(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	api.on(http.MethodGet, "/v2/pods/gone", problem(404, "pod not found"))
	api.on(http.MethodDelete, "/v2/pods/gone", problem(404, "pod not found"))
	api.on(http.MethodGet, "/v2/pods/ended", reply(200, podJSON("ended", "hl-job-a", "TERMINATED")))
	api.on(http.MethodGet, "/v2/pods/p1", reply(200, podJSON("p1", "hl-job-a.up", "RUNNING")))
	api.on(http.MethodDelete, "/v2/pods/p1", reply(204, ""))
	ctx := context.Background()
	for _, id := range []string{"gone", "ended"} {
		if _, exists, err := p.Get(ctx, id); err != nil || exists {
			t.Errorf("Get(%s) = exists %v, %v", id, exists, err)
		}
	}
	if i, exists, err := p.Get(ctx, "p1"); err != nil || !exists || i.Name != "hl-job-a" || i.State != provider.StateRunning {
		t.Errorf("Get(p1) = %+v, %v, %v", i, exists, err)
	}
	for _, id := range []string{"gone", "p1"} {
		if err := p.Destroy(ctx, id); err != nil {
			t.Errorf("Destroy(%s): %v", id, err)
		}
	}
	api.on(http.MethodDelete, "/v2/pods/p1", problem(500, "upstream"))
	if err := p.Destroy(ctx, "p1"); err == nil {
		t.Error("a refused Destroy reported no error")
	}
}

func TestAPodWhoseStartIsNoTimeIsAnError(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	malformed := strings.Replace(podJSON("p1", "hl-job-a", "RUNNING"), "2026-10-24T12:50:40Z", "yesterday", 1)
	api.on(http.MethodGet, "/v2/pods", reply(200, podsPage(malformed)))
	api.on(http.MethodGet, "/v2/pods/p1", reply(200, malformed))
	if _, err := p.List(context.Background(), "hl-job-"); err == nil || !strings.Contains(err.Error(), "pod p1") {
		t.Errorf("List = %v, want the pod's start refused", err)
	}
	if _, found, err := p.Get(context.Background(), "p1"); err == nil || found {
		t.Errorf("Get = %v, %v, want the pod's start refused", found, err)
	}
}
