package e2b

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
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
	api.on(http.MethodGet, "/envd/sb2/files", answer(200, "running\nexited\n"))
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
		state   http.HandlerFunc
		want    provider.State
		expired bool
	}{
		"no state file yet":            {answer(404, noFileJSON), provider.StateStarting, false},
		"a file made and empty":        {answer(200, ""), provider.StateStarting, false},
		"running":                      {answer(200, "running\n"), provider.StateRunning, false},
		"exited":                       {answer(200, "running\nexited\n"), provider.StateExited, false},
		"ended at the plan's lifetime": {answer(200, "running\nexpired\n"), provider.StateExited, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "running"))))
			readable(api, tc.state)
			p := newTestProvider(t, api)

			got, err := p.List(context.Background(), "hl-")
			if err != nil || len(got) != 1 || got[0].State != tc.want || got[0].Expired != tc.expired {
				t.Fatalf("List = %+v, %v, want one instance that is %s, expired %v", got, err, tc.want, tc.expired)
			}
		})
	}
}

func TestAPausedSandboxIsPausedAndNothingInsideItIsRead(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "paused"))))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-")
	if err != nil || len(got) != 1 || got[0].State != provider.StatePaused {
		t.Fatalf("List = %+v, %v, want the paused sandbox listed as paused, so it can be resumed or destroyed", got, err)
	}
	if api.total() != 1 {
		t.Errorf("requests = %d, want the list alone: reading a paused sandbox would resume it", api.total())
	}
}

func TestASandboxInAStateTheAdapterDoesNotKnowIsUnknownAndNothingInsideItIsRead(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "migrating"))))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-")
	if err != nil || len(got) != 1 || got[0].State != provider.StateUnknown {
		t.Fatalf("List = %+v, %v, want the sandbox listed as unknown, never as starting", got, err)
	}
	if api.total() != 1 {
		t.Errorf("requests = %d, want the list alone", api.total())
	}
}

func TestListFailsOnlyWhenTheVendorsOwnListDoes(t *testing.T) {
	for name, list := range map[string]http.HandlerFunc{
		"rate limited": answer(429, `{"code":429,"message":"rate limit exceeded"}`),
		"server error": answer(500, `oops`),
		"not a list":   answer(200, `{"sandboxes":[]}`),
		"too large":    answer(200, "["+strings.Repeat(" ", maxAnswer)+"]"),
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, sandboxesPath, list)
			p := newTestProvider(t, api)

			if got, err := p.List(context.Background(), "hl-"); err == nil {
				t.Fatalf("List = %+v with no error", got)
			}
			if api.total() != 1 {
				t.Errorf("requests = %d, want 1 and no retry", api.total())
			}
		})
	}
}

// A caller creates what it does not find and destroys nothing it cannot see, so one sandbox
// that cannot be read must neither fail the list of all the others nor drop out of it.
func TestListKeepsEverySandboxWhateverReadingItCameTo(t *testing.T) {
	hung := func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	cases := map[string]struct {
		detail, state http.HandlerFunc
		want          provider.State
	}{
		"token rate limited":         {answer(429, `{"code":429}`), nil, provider.StateUnknown},
		"token server error":         {answer(500, `oops`), nil, provider.StateUnknown},
		"token refused":              {answer(401, `{"code":401}`), nil, provider.StateUnknown},
		"token read hangs":           {hung, nil, provider.StateUnknown},
		"shown without a token":      {answer(200, `{"sandboxID":"sb1"}`), nil, provider.StateUnknown},
		"gone when read":             {answer(404, goneJSON), nil, provider.StateUnknown},
		"envd unreachable":           {answer(200, detailJSON), answer(502, deadEnvd), provider.StateUnknown},
		"envd refuses the token":     {answer(200, detailJSON), answer(401, `{"code":401}`), provider.StateUnknown},
		"envd hangs":                 {answer(200, detailJSON), hung, provider.StateUnknown},
		"a state nobody wrote":       {answer(200, detailJSON), answer(200, "exited\n"), provider.StateExited},
		"a third state line":         {answer(200, detailJSON), answer(200, "running\nexited\nrunning\n"), provider.StateExited},
		"expired with nothing run":   {answer(200, detailJSON), answer(200, "expired\n"), provider.StateExited},
		"a state file without end":   {answer(200, detailJSON), answer(200, strings.Repeat("running\n", 1<<20)), provider.StateExited},
		"a state file one byte over": {answer(200, detailJSON), answer(200, strings.Repeat("x", maxStateBytes+1)), provider.StateExited},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "running"), sandboxJSON("sb2", "hl-b", "running"))))
			api.on(http.MethodGet, sandboxPath, tc.detail)
			if tc.state != nil {
				api.on(http.MethodGet, envdFiles, tc.state)
			}
			api.on(http.MethodGet, "/sandboxes/sb2", answer(200, detailJSON))
			api.on(http.MethodGet, "/envd/sb2/files", answer(200, "running\nexited\n"))
			p := newTestProvider(t, api)
			p.stateRead = 100 * time.Millisecond

			got, err := p.List(context.Background(), "hl-")
			if err != nil || len(got) != 2 {
				t.Fatalf("List = %+v, %v, want both sandboxes and no error", got, err)
			}
			if got[0].ID != "sb1" || got[0].Name != "hl-a" || got[0].State != tc.want || got[0].Expired {
				t.Errorf("the sandbox that could not be read = %+v, want it listed as %s and never as expired", got[0], tc.want)
			}
			if got[1].ID != "sb2" || got[1].State != provider.StateExited {
				t.Errorf("the sandbox beside it = %+v, want it read as exited", got[1])
			}
		})
	}
}

func TestListReadsSandboxesSideBySideAndNoMoreThanItsBoundAtOnce(t *testing.T) {
	const count = 40
	api := newFakeAPI(t)
	var listed []string
	for i := range count {
		listed = append(listed, sandboxJSON(fmt.Sprintf("s%d", i), fmt.Sprintf("hl-%d", i), "running"))
	}
	api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(listed...)))
	var mu sync.Mutex
	reading, most := 0, 0
	for i := range count {
		api.on(http.MethodGet, fmt.Sprintf("/sandboxes/s%d", i), func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			reading++
			most = max(most, reading)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			answer(200, detailJSON)(w, r)
		})
		api.on(http.MethodGet, fmt.Sprintf("/envd/s%d/files", i), func(w http.ResponseWriter, r *http.Request) {
			answer(200, "running\n")(w, r)
			mu.Lock()
			reading--
			mu.Unlock()
		})
	}
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-")
	if err != nil || len(got) != count {
		t.Fatalf("List = %d instances, %v, want %d", len(got), err, count)
	}
	for i, instance := range got {
		if want := fmt.Sprintf("s%d", i); instance.ID != want || instance.State != provider.StateRunning {
			t.Fatalf("instance %d = %+v, want %s running, in the order of the vendor's list", i, instance, want)
		}
	}
	if most < 2 || most > stateReaders {
		t.Errorf("%d sandboxes were read at once, want more than one and at most %d", most, stateReaders)
	}
}

func TestListFailsWhenTheCallersTimeEndsBeforeEverySandboxIsRead(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, sandboxesPath, answer(200, sandboxes(sandboxJSON("sb1", "hl-a", "running"))))
	api.on(http.MethodGet, sandboxPath, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	p := newTestProvider(t, api)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if got, err := p.List(ctx, "hl-"); err == nil {
		t.Fatalf("List = %+v with no error, though the caller gave up before the states were read", got)
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
