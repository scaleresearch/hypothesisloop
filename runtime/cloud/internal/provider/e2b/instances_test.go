package e2b

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// readable wires what List reads of sandbox sb1 beyond the list itself: its token and its state
// file.
func readable(api *fakeAPI, state http.HandlerFunc) {
	api.on(http.MethodGet, sandboxPath, answer(200, detailJSON))
	api.on(http.MethodGet, envdFiles, state)
}

func TestListReturnsTheSandboxesOfThePrefixUnderTheirNames(t *testing.T) {
	api := newFakeAPI(t)
	long := "hl-agent-" + strings.Repeat("x", provider.MaxNameLength-len("hl-agent-"))
	api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(
		sandboxJSON("sb1", "hl-agent-pe1-a1", "running"),
		sandboxJSON("sb2", "hl-agent-pe1-a1", "running"),
		sandboxJSON("sb3", long, "running"),
		sandboxJSON("other", "hl-other-pe1-a1", "running"),
		sandboxJSON("foreign", "", "running"),
	)))
	for _, id := range []string{"sb1", "sb2", "sb3"} {
		api.on(http.MethodGet, "/sandboxes/"+id, answer(200, detailJSON))
	}
	api.on(http.MethodGet, "/envd/sb1/files", answer(200, "running\n"))
	api.on(http.MethodGet, "/envd/sb2/files", answer(200, "running\nexited 3\n"))
	api.on(http.MethodGet, "/envd/sb3/files", answer(404, noFileJSON))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-agent-")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	started := time.Date(2026, 10, 5, 11, 0, 0, 500000000, time.UTC)
	want := []provider.Instance{
		{ID: "sb1", Name: "hl-agent-pe1-a1", State: provider.StateRunning, StartedAt: started},
		{ID: "sb2", Name: "hl-agent-pe1-a1", State: provider.StateExited, StartedAt: started},
		{ID: "sb3", Name: long, State: provider.StateStarting, StartedAt: started},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List\n got %+v\nwant %+v", got, want)
	}
	lists := api.sent(http.MethodGet, sandboxesPath)
	if len(lists) != 1 || lists[0].Query != "limit=100&state=running%2Cpaused" || lists[0].APIKey != testKey {
		t.Errorf("lists = %+v, want one page of running and paused sandboxes", lists)
	}
	state := api.sent(http.MethodGet, "/envd/sb1/files")
	if len(state) != 1 || state[0].Query != "path=%2Fvar%2Ftmp%2F.hl-supervisor%2Fstate" || state[0].AccessToken != testToken || state[0].APIKey != "" {
		t.Errorf("state reads = %+v, want the state file, read with the sandbox's token and without the API key", state)
	}
	if api.total() != 7 {
		t.Errorf("requests = %d, want 7: the list, and a token and a state for each sandbox of the prefix", api.total())
	}
}

func TestListFollowsEveryPage(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxesPath, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("nextToken") {
		case "":
			w.Header().Set("X-Next-Token", "page-2")
			answer(200, sandboxes(sandboxJSON("other", "zz-other", "running")))(w, r)
		case "page-2":
			w.Header().Set("X-Next-Token", "page-3")
			answer(200, sandboxes())(w, r)
		case "page-3":
			answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "running")))(w, r)
		}
	})
	readable(api, answer(200, "running\n"))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-")
	if err != nil || len(got) != 1 || got[0].ID != "sb1" {
		t.Fatalf("List = %+v, %v, want the sandbox of the third page", got, err)
	}
	if pages := len(api.sent(http.MethodGet, sandboxesPath)); pages != 3 {
		t.Errorf("pages = %d, want 3: an empty page with a token is not the last", pages)
	}
}

func TestListOfATeamWithNoSandboxesIsEmpty(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxesPath, answer(200, `[]`))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("List = %#v, %v, want an empty list", got, err)
	}
}

func TestListMapsWhatTheSupervisorWrote(t *testing.T) {
	cases := map[string]struct {
		state http.HandlerFunc
		want  provider.State
	}{
		"no state file yet":     {answer(404, noFileJSON), provider.StateStarting},
		"a file made and empty": {answer(200, ""), provider.StateStarting},
		"running":               {answer(200, "running\n"), provider.StateRunning},
		"exited with success":   {answer(200, "running\nexited 0\n"), provider.StateExited},
		"exited with a signal":  {answer(200, "running\nexited 143\n"), provider.StateExited},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "running"))))
			readable(api, tc.state)
			p := newTestProvider(t, api)

			got, err := p.List(context.Background(), "hl-")
			if err != nil || len(got) != 1 || got[0].State != tc.want {
				t.Fatalf("List = %+v, %v, want one instance that is %s", got, err, tc.want)
			}
		})
	}
}

func TestAPausedSandboxIsStartingAndNothingInsideItIsRead(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "paused"))))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-")
	if err != nil || len(got) != 1 || got[0].State != provider.StateStarting {
		t.Fatalf("List = %+v, %v, want the paused sandbox listed as starting, so it can be destroyed", got, err)
	}
	if api.total() != 1 {
		t.Errorf("requests = %d, want the list alone: reading a paused sandbox would resume it", api.total())
	}
}

func TestASandboxThatEndedBetweenTheListAndTheReadIsGone(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "running"), sandboxJSON("sb2", "hl-b", "running"))))
	api.on(http.MethodGet, sandboxPath, answer(404, goneJSON))
	api.on(http.MethodGet, "/sandboxes/sb2", answer(200, detailJSON))
	api.on(http.MethodGet, "/envd/sb2/files", answer(200, "running\n"))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-")
	if err != nil || len(got) != 1 || got[0].ID != "sb2" {
		t.Fatalf("List = %+v, %v, want only the sandbox that is still there", got, err)
	}
}

func TestListReportsEveryFailure(t *testing.T) {
	listed := answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "running")))
	cases := map[string]struct {
		list, detail, state http.HandlerFunc
		requests            int
	}{
		"list rate limited":      {list: answer(429, `{"code":429,"message":"rate limit exceeded"}`), requests: 1},
		"list server error":      {list: answer(500, `oops`), requests: 1},
		"list is not a list":     {list: answer(200, `{"sandboxes":[]}`), requests: 1},
		"token rate limited":     {list: listed, detail: answer(429, `{"code":429}`), requests: 2},
		"token server error":     {list: listed, detail: answer(500, `oops`), requests: 2},
		"shown without a token":  {list: listed, detail: answer(200, `{"sandboxID":"sb1"}`), requests: 2},
		"envd unreachable":       {list: listed, detail: answer(200, detailJSON), state: answer(502, deadEnvd), requests: 3},
		"envd refuses the token": {list: listed, detail: answer(200, detailJSON), state: answer(401, `{"code":401}`), requests: 3},
		"a state nobody wrote":   {list: listed, detail: answer(200, detailJSON), state: answer(200, "exited 0\n"), requests: 3},
		"a third state line":     {list: listed, detail: answer(200, detailJSON), state: answer(200, "running\nexited 0\nrunning\n"), requests: 3},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, sandboxesPath, tc.list)
			if tc.detail != nil {
				api.on(http.MethodGet, sandboxPath, tc.detail)
			}
			if tc.state != nil {
				api.on(http.MethodGet, envdFiles, tc.state)
			}
			p := newTestProvider(t, api)

			if got, err := p.List(context.Background(), "hl-"); err == nil {
				t.Fatalf("List = %+v with no error", got)
			}
			if api.total() != tc.requests {
				t.Errorf("requests = %d, want %d and no retry", api.total(), tc.requests)
			}
		})
	}
}

func TestDestroyKillsTheSandbox(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodDelete, sandboxPath, answer(204, ``))
	p := newTestProvider(t, api)

	if err := p.Destroy(context.Background(), "sb1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if sent := api.sent(http.MethodDelete, sandboxPath); len(sent) != 1 || sent[0].APIKey != testKey {
		t.Errorf("deletes = %+v, want one", sent)
	}
}

func TestDestroyOfAGoneSandboxIsNotAnError(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodDelete, sandboxPath, answer(404, goneJSON))
	p := newTestProvider(t, api)

	if err := p.Destroy(context.Background(), "sb1"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func TestDestroyReportsEveryOtherFailure(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"rate limited": answer(429, `{"code":429}`),
		"server error": answer(500, `oops`),
		"bad key":      answer(401, `{"code":401}`),
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodDelete, sandboxPath, handler)
			p := newTestProvider(t, api)

			if err := p.Destroy(context.Background(), "sb1"); err == nil {
				t.Fatal("no error")
			}
			if api.total() != 1 {
				t.Errorf("requests = %d, want 1 and no retry", api.total())
			}
		})
	}
}
