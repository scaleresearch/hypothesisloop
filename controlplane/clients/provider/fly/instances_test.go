package fly

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

func TestListReturnsTheMachinesOfThePrefixUnderTheirNames(t *testing.T) {
	api := newFakeAPI(t)
	listsTheApp(api)
	long := "hl-agent-" + strings.Repeat("x", provider.MaxNameLength-len("hl-agent-"))
	api.on(http.MethodGet, machinesPath, answer(200, machines(
		machineJSON("1", "hl-agent-pe1-a1", "started", 0),
		machineJSON("2", "hl-agent-pe1-a1", "started", 0),
		machineJSON("3", long, "created", 0),
		machineJSON("4", "hl-other-pe1-a1", "started", 0),
		machineJSON("5", "", "started", 0),
	)))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-agent-")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	started := time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC)
	want := []provider.Instance{
		{ID: testApp + "/1", Name: "hl-agent-pe1-a1", State: provider.StateRunning, StartedAt: started, Architecture: "amd64"},
		{ID: testApp + "/2", Name: "hl-agent-pe1-a1", State: provider.StateRunning, StartedAt: started, Architecture: "amd64"},
		{ID: testApp + "/3", Name: long, State: provider.StateStarting, StartedAt: started, Architecture: "amd64"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List\n got %+v\nwant %+v", got, want)
	}
	lists := api.sent(http.MethodGet, machinesPath)
	if len(lists) != 1 || lists[0].Query != "" || lists[0].Auth != testToken {
		t.Errorf("lists = %+v, want one call for the whole app", lists)
	}
	apps := api.sent(http.MethodGet, appsPath)
	if len(apps) != 1 || apps[0].Query != "org_slug="+testOrg || apps[0].Auth != testToken {
		t.Errorf("app lists = %+v, want one of the organization", apps)
	}
	if api.total() != 2 {
		t.Errorf("requests = %d, want the app list and the machines of the one app of the prefix", api.total())
	}
}

func TestListOfAnEmptyAppIsEmpty(t *testing.T) {
	api := newFakeAPI(t)
	listsTheApp(api)
	api.on(http.MethodGet, machinesPath, answer(200, `[]`))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("List = %#v, %v, want an empty list", got, err)
	}
}

func TestListMapsEveryMachineState(t *testing.T) {
	cases := map[string]struct {
		state  provider.State
		listed bool
	}{
		"created":    {provider.StateStarting, true},
		"creating":   {provider.StateStarting, true},
		"starting":   {provider.StateStarting, true},
		"restarting": {provider.StateUnknown, true},
		"updating":   {provider.StateUnknown, true},
		"replacing":  {provider.StateUnknown, true},
		"started":    {provider.StateRunning, true},
		"stopping":   {provider.StateRunning, true},
		"destroying": {"", false},
		"destroyed":  {"", false},
		"suspended":  {provider.StatePaused, true},
		"suspending": {provider.StateRunning, true},
		"brand-new":  {provider.StateUnknown, true},
	}
	for vendorState, want := range cases {
		t.Run(vendorState, func(t *testing.T) {
			api := newFakeAPI(t)
			listsTheApp(api)
			api.on(http.MethodGet, machinesPath, answer(200, machines(machineJSON("9001", "hl-a", vendorState, time.Hour))))
			p := newTestProvider(t, api)

			got, err := p.List(context.Background(), "hl-")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if !want.listed {
				if len(got) != 0 {
					t.Fatalf("List = %+v, want a machine being destroyed to be gone", got)
				}
				return
			}
			if len(got) != 1 || got[0].State != want.state {
				t.Fatalf("List = %+v, want one instance that is %s", got, want.state)
			}
			if api.total() != 2 {
				t.Errorf("requests = %d, want the lists alone: only a stopped machine's log is read", api.total())
			}
		})
	}
}

const (
	shutdownLine = "\x1b[32m INFO\x1b[0m Main child exited normally with code: 3"
	policyLine   = "machine restart policy set to 'no', not restarting"
)

func TestAStoppedMachineIsExitedOnlyOnceItsOutputIsComplete(t *testing.T) {
	cases := map[string]struct {
		vendorState string
		stoppedAgo  time.Duration
		log         http.HandlerFunc
		want        provider.State
		logReads    int
	}{
		"the end mark is in the log": {"stopped", 2 * time.Second,
			answer(200, logJSON("hl-out|last line", "hl-ended", shutdownLine, policyLine)), provider.StateExited, 1},
		"the log trails the machine": {"stopped", 2 * time.Second,
			answer(200, logJSON("hl-out|not the last line")), provider.StateRunning, 1},
		"the log is still empty": {"stopped", 2 * time.Second,
			answer(200, logJSON()), provider.StateRunning, 1},
		"the command printed the mark itself": {"stopped", 2 * time.Second,
			answer(200, logJSON("hl-out|hl-ended", "hl-ended and more")), provider.StateRunning, 1},
		"the end mark is of an earlier start of the machine": {"stopped", 2 * time.Second,
			answer(200, logJSON("hl-started", "hl-out|first run", "hl-ended", shutdownLine, "hl-started", "hl-out|second run")), provider.StateRunning, 1},
		"the machine was started again and ended again": {"stopped", 2 * time.Second,
			answer(200, logJSON("hl-started", "hl-ended", shutdownLine, "hl-started", "hl-out|second run", "hl-ended")), provider.StateExited, 1},
		"the log cannot be read": {"stopped", 2 * time.Second,
			answer(503, `unavailable`), provider.StateRunning, 1},
		"stopped without a mark, a while ago": {"stopped", logSettle + time.Second,
			nil, provider.StateExited, 0},
		"stopped without a mark, just now": {"stopped", logSettle - time.Second,
			answer(200, logJSON(shutdownLine)), provider.StateRunning, 1},
		"failed to start, a while ago": {"failed", logSettle + time.Second,
			nil, provider.StateExited, 0},
		"failed to start, just now": {"failed", time.Second,
			answer(200, logJSON()), provider.StateRunning, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			listsTheApp(api)
			api.on(http.MethodGet, machinesPath, answer(200, machines(machineJSON("9001", "hl-a", tc.vendorState, tc.stoppedAgo))))
			if tc.log != nil {
				api.on(http.MethodGet, logsPath, tc.log)
			}
			p := newTestProvider(t, api)

			got, err := p.List(context.Background(), "hl-")
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != 1 || got[0].State != tc.want {
				t.Fatalf("List = %+v, want one instance that is %s", got, tc.want)
			}
			reads := api.sent(http.MethodGet, logsPath)
			if len(reads) != tc.logReads {
				t.Fatalf("log reads = %d, want %d", len(reads), tc.logReads)
			}
			if tc.logReads == 1 && reads[0].Query != "instance=9001&limit=50" {
				t.Errorf("log read = %q, want the machine's last entries", reads[0].Query)
			}
		})
	}
}

func TestListReadsNoLogOfAStoppedMachineOutsideThePrefix(t *testing.T) {
	api := newFakeAPI(t)
	listsTheApp(api)
	api.on(http.MethodGet, machinesPath, answer(200, machines(machineJSON("9001", "hl-other", "stopped", time.Second))))
	p := newTestProvider(t, api)

	if got, err := p.List(context.Background(), "hl-agent-"); err != nil || len(got) != 0 {
		t.Fatalf("List = %+v, %v", got, err)
	}
	if api.total() != 2 {
		t.Errorf("requests = %d, want the lists alone", api.total())
	}
}

func TestListReadsEveryAppOfThePrefixAndNoOther(t *testing.T) {
	api := newFakeAPI(t)
	other := testPrefix + "-ffeeddccbbaa9988"
	api.on(http.MethodGet, appsPath, answer(200, appsJSON(testApp, "hypothesisloop-agents", other, testPrefix+"-short", testPrefix+"-FFEEDDCCBBAA9988")))
	api.on(http.MethodGet, machinesPath, answer(200, machines(machineJSON("2", "hl-agent-b", "started", 0))))
	api.on(http.MethodGet, appsPath+"/"+other+"/machines", answer(200, machines(machineJSON("1", "hl-agent-a", "started", 0))))
	p := newTestProvider(t, api)

	got, err := p.List(context.Background(), "hl-agent-")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].ID != testApp+"/2" || got[0].Name != "hl-agent-b" || got[1].ID != other+"/1" || got[1].Name != "hl-agent-a" {
		t.Errorf("List = %+v, want the machine of each app of the prefix by app and machine id", got)
	}
	if api.total() != 3 {
		t.Errorf("requests = %d, want the app list and one list of each app of the prefix", api.total())
	}
}

func TestListTakesTheNameFromTheEnvAndNeverFromMetadata(t *testing.T) {
	api := newFakeAPI(t)
	listsTheApp(api)
	api.on(http.MethodGet, machinesPath, answer(200, machines(machineJSON("1", "", "started", 0))))
	p := newTestProvider(t, api)

	if got, err := p.List(context.Background(), "hl-agent-"); err != nil || len(got) != 0 {
		t.Fatalf("List = %+v, %v, want nothing: the machine names itself only in metadata, which it can write", got, err)
	}
}

func TestListOfAnAppDeletedSinceTheAppListHasNoMachine(t *testing.T) {
	api := newFakeAPI(t)
	listsTheApp(api)
	api.on(http.MethodGet, machinesPath, answer(404, `{"error":"app not found"}`))
	p := newTestProvider(t, api)

	if got, err := p.List(context.Background(), "hl-"); err != nil || len(got) != 0 {
		t.Fatalf("List = %+v, %v, want an empty list", got, err)
	}
}

func TestListRefusesAnAppListThatLeavesAppsOut(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, appsPath, answer(200, `{"total_apps":3,"apps":[{"name":"`+testApp+`"}]}`))
	p := newTestProvider(t, api)

	if _, err := p.List(context.Background(), "hl-"); err == nil || !strings.Contains(err.Error(), "holds 1 of the 3 apps") {
		t.Fatalf("err = %v, want one about the apps left out", err)
	}
}

func TestListReportsEveryFailure(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"rate limited":  answer(429, `{"error":"rate limit exceeded"}`),
		"server error":  answer(500, `oops`),
		"unauthorized":  answer(401, `{"error":"unauthorized"}`),
		"not a list":    answer(200, `{"machines":[]}`),
		"no date given": func(w http.ResponseWriter, _ *http.Request) { w.Header()["Date"] = nil; _, _ = w.Write([]byte(`[]`)) },
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			listsTheApp(api)
			api.on(http.MethodGet, machinesPath, handler)
			p := newTestProvider(t, api)

			if got, err := p.List(context.Background(), "hl-"); err == nil {
				t.Fatalf("List = %+v with no error", got)
			}
			if api.total() != 2 {
				t.Errorf("requests = %d, want the two lists and no retry", api.total())
			}
		})
	}
}

func TestDestroyDeletesTheMachinesAppWhateverItsState(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodDelete, appPath, answer(202, ``))
	p := newTestProvider(t, api)

	if err := p.Destroy(context.Background(), testID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if sent := api.sent(http.MethodDelete, appPath); len(sent) != 1 || sent[0].Auth != testToken {
		t.Errorf("deletes = %+v, want one of the app", sent)
	}
}

func TestDestroyOfAGoneMachineIsNotAnError(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodDelete, appPath, answer(404, `{"error":"app not found"}`))
	if err := newTestProvider(t, api).Destroy(context.Background(), testID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func TestEveryCallRefusesAnIDOutsideTheAppsOfThePrefix(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{"9001", "hypothesisloop-agents/9001", testPrefix + "-short/9001", testApp + "/", testApp + "/90/01", "other-" + testSuffix + "/9001"} {
		api := newFakeAPI(t)
		p := newTestProvider(t, api)
		if _, exists, err := p.Get(ctx, id); exists || err != nil {
			t.Errorf("Get(%q) = %v, %v, want a machine that is not this adapter's to be gone", id, exists, err)
		}
		_, logsErr := p.Logs(ctx, id, 1)
		for name, err := range map[string]error{
			"Destroy": p.Destroy(ctx, id), "Stop": p.Stop(ctx, id), "Pause": p.Pause(ctx, id), "Resume": p.Resume(ctx, id),
			"Logs": logsErr, "StartWarm": p.StartWarm(ctx, id, agentSpec()),
		} {
			if err == nil || !strings.Contains(err.Error(), "is not the id of a machine") {
				t.Errorf("%s(%q) = %v, want a refusal", name, id, err)
			}
		}
		if api.total() != 0 {
			t.Errorf("id %q: %d requests were sent", id, api.total())
		}
	}
}

func TestDestroyReportsEveryOtherFailure(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"rate limited": answer(429, `{"error":"rate limit exceeded"}`),
		"server error": answer(500, `oops`),
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodDelete, appPath, handler)
			p := newTestProvider(t, api)

			if err := p.Destroy(context.Background(), testID); err == nil {
				t.Fatal("no error")
			}
			if api.total() != 1 {
				t.Errorf("requests = %d, want 1 and no retry", api.total())
			}
		})
	}
}
