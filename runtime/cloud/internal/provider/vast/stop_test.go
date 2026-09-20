package vast

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

const instancePath = "/api/v0/instances/9001/"

func shown(label string) http.HandlerFunc {
	return answer(200, `{"instances":`+instanceJSON(9001, label, "running")+`}`)
}

func TestStopMarksTheLabelAndLeavesTheInstanceRunning(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, instancePath, shown("hl-exp1-a2"))
	api.on(http.MethodPut, instancePath, answer(200, `{"success":true}`))
	p := newTestProvider(t, api, Settings{})

	if err := p.Stop(context.Background(), "9001"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	read := api.sent(http.MethodGet, instancePath)
	if len(read) != 1 || read[0].Query != "owner=me" || read[0].Auth != "Bearer "+testKey {
		t.Errorf("reads = %+v", read)
	}
	sent := api.sent(http.MethodPut, instancePath)
	if len(sent) != 1 || !reflect.DeepEqual(sent[0].Body, map[string]any{"label": "hl-exp1-a2.stop"}) {
		t.Errorf("writes = %+v, want only the label with the mark: the vendor's own stop kills the command", sent)
	}
}

func TestStopOfAMarkedInstanceWritesNothing(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, instancePath, shown("hl-exp1-a2.stop"))
	p := newTestProvider(t, api, Settings{})

	for range 3 {
		if err := p.Stop(context.Background(), "9001"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
	if got := len(api.sent(http.MethodPut, instancePath)); got != 0 {
		t.Errorf("writes = %d, want 0", got)
	}
}

func TestStopOfAGoneInstanceIsNotAnError(t *testing.T) {
	t.Run("gone at the read", func(t *testing.T) {
		api := newFakeAPI(t)
		api.on(http.MethodGet, instancePath, answer(200, `{"instances":null}`))
		p := newTestProvider(t, api, Settings{})

		if err := p.Stop(context.Background(), "9001"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if api.total() != 1 {
			t.Errorf("requests = %d, want the one read", api.total())
		}
	})
	t.Run("gone between the read and the write", func(t *testing.T) {
		api := newFakeAPI(t)
		api.on(http.MethodGet, instancePath, shown("hl-exp1-a2"))
		api.on(http.MethodPut, instancePath, answer(404, gone))
		p := newTestProvider(t, api, Settings{})

		if err := p.Stop(context.Background(), "9001"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})
}

func TestStopReportsEveryOtherFailure(t *testing.T) {
	cases := map[string]struct {
		read, write http.HandlerFunc
		requests    int
	}{
		"read rate limited":  {read: answer(429, `API requests too frequent`), requests: 1},
		"read server error":  {read: answer(500, `oops`), requests: 1},
		"no label":           {read: answer(200, `{"instances":`+instanceJSON(9001, "", "running")+`}`), requests: 1},
		"write rate limited": {read: shown("hl-exp1-a2"), write: answer(429, `API requests too frequent`), requests: 2},
		"write server error": {read: shown("hl-exp1-a2"), write: answer(503, `unavailable`), requests: 2},
		"refusal in a 200":   {read: shown("hl-exp1-a2"), write: answer(200, `{"success":false,"msg":"locked"}`), requests: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, instancePath, tc.read)
			if tc.write != nil {
				api.on(http.MethodPut, instancePath, tc.write)
			}
			p := newTestProvider(t, api, Settings{})

			if err := p.Stop(context.Background(), "9001"); err == nil {
				t.Fatal("no error")
			}
			if api.total() != tc.requests {
				t.Errorf("requests = %d, want %d", api.total(), tc.requests)
			}
		})
	}
}
