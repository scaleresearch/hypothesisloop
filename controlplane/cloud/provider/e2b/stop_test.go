package e2b

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

func TestStopHasEnvdSendTheSupervisorSIGTERMAndNothingElse(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxPath, answer(200, detailJSON))
	api.on(http.MethodPost, envdSignal, answer(200, `{}`))
	p := newTestProvider(t, api)

	for range 3 {
		if err := p.Stop(context.Background(), "sb1"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
	signals := api.sent(http.MethodPost, envdSignal)
	if len(signals) != 3 || api.total() != 6 {
		t.Fatalf("requests = %d, of them signals = %d, want a token read and a signal for each Stop: killing the sandbox would kill the command", api.total(), len(signals))
	}
	for _, signal := range signals {
		want := map[string]any{"process": map[string]any{"tag": "hl-supervisor"}, "signal": "SIGNAL_SIGTERM"}
		if !reflect.DeepEqual(signal.json(t), want) || signal.AccessToken != testToken || signal.ContentType != "application/json" {
			t.Errorf("signal = %s with token %q", signal.Raw, signal.AccessToken)
		}
	}
}

func TestStopOfASandboxWithNothingToSignalIsNotAnError(t *testing.T) {
	t.Run("the sandbox is gone", func(t *testing.T) {
		api := newFakeAPI(t)
		api.on(http.MethodGet, sandboxPath, answer(404, goneJSON))
		p := newTestProvider(t, api)

		if err := p.Stop(context.Background(), "sb1"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if api.total() != 1 {
			t.Errorf("requests = %d, want the one read", api.total())
		}
	})
	t.Run("the command has ended or was never started", func(t *testing.T) {
		api := newFakeAPI(t)
		api.on(http.MethodGet, sandboxPath, answer(200, detailJSON))
		api.on(http.MethodPost, envdSignal, answer(404, `{"code":"not_found","message":"process with tag hl-supervisor not found"}`))
		p := newTestProvider(t, api)

		if err := p.Stop(context.Background(), "sb1"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})
}

func TestStopReportsEveryOtherFailure(t *testing.T) {
	detail := answer(200, detailJSON)
	cases := map[string]struct {
		detail, signal http.HandlerFunc
		requests       int
	}{
		"token rate limited":     {detail: answer(429, `{"code":429}`), requests: 1},
		"token server error":     {detail: answer(500, `oops`), requests: 1},
		"envd unreachable":       {detail: detail, signal: answer(502, deadEnvd), requests: 2},
		"envd refuses the token": {detail: detail, signal: answer(401, `{"code":"unauthenticated"}`), requests: 2},
		"envd fails":             {detail: detail, signal: answer(500, `{"code":"internal","message":"error sending signal"}`), requests: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, sandboxPath, tc.detail)
			if tc.signal != nil {
				api.on(http.MethodPost, envdSignal, tc.signal)
			}
			p := newTestProvider(t, api)

			if err := p.Stop(context.Background(), "sb1"); err == nil {
				t.Fatal("no error")
			}
			if api.total() != tc.requests {
				t.Errorf("requests = %d, want %d and no retry", api.total(), tc.requests)
			}
		})
	}
}
