package runpod

import (
	"context"
	"net/http"
	"testing"
)

func TestStopMarksTheNameOnceAndLeavesAnEndedPodAlone(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	ctx := context.Background()
	api.on(http.MethodPatch, "/v2/pods/p1", reply(200, podJSON("p1", "hl-job-a.stop.up", "RUNNING")))
	for name, wantPatch := range map[string]string{
		"hl-job-a":             "hl-job-a.stop",
		"hl-job-a.up":          "hl-job-a.stop.up",
		"hl-job-a.stop.up":     "",
		"hl-job-a.exited":      "",
		"hl-job-a.stop.exited": "",
	} {
		api.mu.Lock()
		api.requests = nil
		api.mu.Unlock()
		api.on(http.MethodGet, "/v2/pods/p1", reply(200, podJSON("p1", name, "RUNNING")))
		if err := p.Stop(ctx, "p1"); err != nil {
			t.Fatalf("Stop of %s: %v", name, err)
		}
		patched := api.sent(http.MethodPatch, "/v2/pods/p1")
		switch {
		case wantPatch == "" && len(patched) > 0:
			t.Errorf("Stop of %s renamed it: %+v", name, patched)
		case wantPatch != "" && (len(patched) != 1 || patched[0].Body["name"] != wantPatch || len(patched[0].Body) != 1):
			t.Errorf("Stop of %s sent %+v, want the name %s alone", name, patched, wantPatch)
		}
	}
	api.on(http.MethodGet, "/v2/pods/gone", problem(404, "pod not found"))
	if err := p.Stop(ctx, "gone"); err != nil {
		t.Errorf("Stop of a pod that is gone: %v", err)
	}
}
