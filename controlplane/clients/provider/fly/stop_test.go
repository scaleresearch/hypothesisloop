package fly

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

const signalPath = machinesPath + "/9001/signal"

func TestStopSignalsTheMachineWithSIGTERMAndNothingElse(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, signalPath, answer(200, `{"ok":true}`))
	p := newTestProvider(t, api)

	for range 3 {
		if err := p.Stop(context.Background(), testID); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
	sent := api.sent(http.MethodPost, signalPath)
	if len(sent) != 3 || api.total() != 3 {
		t.Fatalf("requests = %d, of them signals = %d, want three signals: the vendor's stop would kill the command", api.total(), len(sent))
	}
	for _, signal := range sent {
		if !reflect.DeepEqual(signal.Body, map[string]any{"signal": "SIGTERM"}) || signal.Auth != testToken {
			t.Errorf("signal = %+v", signal)
		}
	}
}

func TestStopOfAMachineWithNothingToSignalIsNotAnError(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"stopped":      answer(200, `{"ok":true}`),
		"gone":         answer(404, `{"error":"machine not found"}`),
		"not yet up":   answer(412, `{"error":"failed_precondition: machine not in known state for signaling, created"}`),
		"empty answer": answer(200, ``),
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPost, signalPath, handler)
			p := newTestProvider(t, api)

			if err := p.Stop(context.Background(), testID); err != nil {
				t.Fatalf("Stop: %v", err)
			}
		})
	}
}

func TestStopReportsEveryOtherFailure(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"rate limited": answer(429, `{"error":"rate limit exceeded"}`),
		"server error": answer(500, `oops`),
		"bad request":  answer(400, `{"error":"invalid signal"}`),
		"unauthorized": answer(401, `{"error":"unauthorized"}`),
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPost, signalPath, handler)
			p := newTestProvider(t, api)

			if err := p.Stop(context.Background(), testID); err == nil {
				t.Fatal("no error")
			}
			if api.total() != 1 {
				t.Errorf("requests = %d, want 1 and no retry", api.total())
			}
		})
	}
}
