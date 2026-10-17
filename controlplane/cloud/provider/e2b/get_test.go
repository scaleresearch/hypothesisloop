package e2b

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
)

func TestGetReadsTheSandboxItselfAsListWouldShowIt(t *testing.T) {
	api := newFakeAPI(t)
	detail := sandboxJSON("sb1", "hl-agent-pe1-a1", "running")
	detail = detail[:len(detail)-1] + `,"envdAccessToken":"` + testToken + `"}`
	api.on(http.MethodGet, sandboxPath, answer(200, detail))
	api.on(http.MethodGet, envdFiles, answer(200, "running\n"))
	p := newTestProvider(t, api)

	got, exists, err := p.Get(context.Background(), "sb1")
	want := provider.Instance{ID: "sb1", Name: "hl-agent-pe1-a1", State: provider.StateRunning, StartedAt: time.Date(2026, 10, 5, 11, 0, 0, 500000000, time.UTC)}
	if err != nil || !exists || !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %+v, %v, %v, want %+v", got, exists, err, want)
	}
	if lists := len(api.sent(http.MethodGet, sandboxesPath)); lists != 0 {
		t.Errorf("lists = %d, want none", lists)
	}
}

func TestGetSaysASandboxIsGoneOnlyWhenTheVendorDoes(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxPath, answer(404, goneJSON))
	if _, exists, err := newTestProvider(t, api).Get(context.Background(), "sb1"); err != nil || exists {
		t.Fatalf("Get = %v, %v, want gone", exists, err)
	}

	api = newFakeAPI(t)
	api.on(http.MethodGet, sandboxPath, answer(500, `oops`))
	if _, exists, err := newTestProvider(t, api).Get(context.Background(), "sb1"); err == nil || exists {
		t.Fatalf("Get = %v, %v, want an error that says neither", exists, err)
	}
}
