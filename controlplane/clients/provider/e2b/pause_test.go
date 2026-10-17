package e2b

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const (
	pausePath   = "/sandboxes/sb1/pause"
	connectPath = "/v2/sandboxes/sb1/connect"
)

func TestPauseIsRepeatableAndASandboxThatIsGoneIsNotAnError(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, pausePath, inSequence(
		answer(204, ``),
		answer(409, `{"code":409,"message":"sandbox is already paused"}`),
		answer(404, `{"code":404,"message":"not found"}`),
		answer(500, `{"code":500,"message":"snapshot failed"}`),
	))
	p := newTestProvider(t, api)

	for _, what := range []string{"running", "paused already", "gone"} {
		if err := p.Pause(context.Background(), "sb1"); err != nil {
			t.Fatalf("Pause of a sandbox that is %s: %v", what, err)
		}
	}
	if err := p.Pause(context.Background(), "sb1"); err == nil || !strings.Contains(err.Error(), "snapshot failed") {
		t.Fatalf("Pause the vendor failed: err = %v, want the vendor's words", err)
	}
	if sent := api.sent(http.MethodPost, pausePath); len(sent) != 4 || sent[0].APIKey != testKey {
		t.Fatalf("pauses = %+v", sent)
	}
}

func TestResumeConnectsWithAWholeLifetimeAndSaysWhenTheSandboxIsGone(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, connectPath, inSequence(
		answer(201, sandboxJSON("sb1", "hl-a", "running")),
		answer(200, sandboxJSON("sb1", "hl-a", "running")),
		answer(404, `{"code":404,"message":"not found"}`),
	))
	p := newTestProvider(t, api)

	for _, what := range []string{"paused", "running"} {
		if err := p.Resume(context.Background(), "sb1"); err != nil {
			t.Fatalf("Resume of a sandbox that is %s: %v", what, err)
		}
	}
	if err := p.Resume(context.Background(), "sb1"); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("Resume of a sandbox that is gone: err = %v", err)
	}
	sent := api.sent(http.MethodPost, connectPath)
	if len(sent) != 3 || sent[0].json(t)["timeout"] != float64(p.cfg.LifetimeSeconds) {
		t.Fatalf("connects = %+v, want the adapter's lifetime asked for", sent)
	}
}
