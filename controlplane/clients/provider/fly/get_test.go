package fly

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

func TestGetReadsTheMachineItselfAsListWouldShowIt(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, machinesPath+"/9001", answer(200, machineJSON("9001", "hl-agent-pe1-a1", "started", 0)))
	p := newTestProvider(t, api)

	got, exists, err := p.Get(context.Background(), "9001")
	want := provider.Instance{ID: "9001", Name: "hl-agent-pe1-a1", State: provider.StateRunning, StartedAt: time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC)}
	if err != nil || !exists || !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %+v, %v, %v, want %+v", got, exists, err, want)
	}
	if sent := api.sent(http.MethodGet, machinesPath+"/9001"); len(sent) != 1 || sent[0].Auth != testToken {
		t.Errorf("reads = %+v", sent)
	}
	if lists := len(api.sent(http.MethodGet, machinesPath)); lists != 0 {
		t.Errorf("lists = %d, want none", lists)
	}
}

func TestGetSaysAMachineIsGoneOnlyWhenTheVendorDoes(t *testing.T) {
	for name, read := range map[string]http.HandlerFunc{
		"not found":       answer(404, `{"error":"machine not found"}`),
		"destroyed":       answer(200, machineJSON("9001", "hl-agent-pe1-a1", "destroyed", 0)),
		"being destroyed": answer(200, machineJSON("9001", "hl-agent-pe1-a1", "destroying", 0)),
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, machinesPath+"/9001", read)
			if _, exists, err := newTestProvider(t, api).Get(context.Background(), "9001"); err != nil || exists {
				t.Fatalf("Get = %v, %v, want gone", exists, err)
			}
		})
	}
	for name, read := range map[string]http.HandlerFunc{
		"server error": answer(500, `oops`),
		"rate limited": answer(429, `{"error":"rate limit exceeded"}`),
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, machinesPath+"/9001", read)
			if _, exists, err := newTestProvider(t, api).Get(context.Background(), "9001"); err == nil || exists {
				t.Fatalf("Get = %v, %v, want an error that says neither", exists, err)
			}
		})
	}
}
