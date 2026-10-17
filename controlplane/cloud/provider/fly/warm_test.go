package fly

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
)

// warmMachineJSON is a Machine Warm made, as the list shows it.
func warmMachineJSON(id, name, state string) string {
	encoded, _ := json.Marshal(map[string]any{
		"id": id, "state": state, "created_at": "2026-10-05T11:00:00Z", "updated_at": apiNow.Format(time.RFC3339),
		"config": map[string]any{"metadata": map[string]string{nameKey: name, warmKey: "1"}},
	})
	return string(encoded)
}

func TestWarmCreatesAStoppedMachineOfTheSpecsImageAndSizeThatRunsNothing(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, machinesPath, answer(200, `{"id":"9001","state":"created"}`))
	api.on(http.MethodGet, machinesPath, answer(200, machines(warmMachineJSON("9001", "hl-agent-pe1-pool-k", "created"))))
	p := newTestProvider(t, api)
	spec := agentSpec()
	spec.Name = "hl-agent-pe1-pool-k"

	id, err := p.Warm(context.Background(), spec)
	if err != nil || id != "9001" {
		t.Fatalf("Warm = %q, %v", id, err)
	}
	made := api.sent(http.MethodPost, machinesPath)
	want := map[string]any{
		"region":      "ams",
		"skip_launch": true,
		"config": map[string]any{
			"image":        "registry.example/agent:1",
			"init":         map[string]any{"exec": []any{"/bin/sh", "-c", "exit 0"}},
			"env":          map[string]any{},
			"metadata":     map[string]any{"hl_name": "hl-agent-pe1-pool-k", "hl_warm": "1"},
			"guest":        map[string]any{"cpu_kind": "shared", "cpus": 2.0, "memory_mb": 4096.0},
			"rootfs":       map[string]any{"size_gb": 11.0},
			"restart":      map[string]any{"policy": "no"},
			"auto_destroy": false,
		},
	}
	if len(made) != 1 || !reflect.DeepEqual(made[0].Body, want) {
		t.Fatalf("warm body\n got %v\nwant %v", made, want)
	}
	if strings.Contains(made[0].Query, "skip") {
		t.Errorf("skip_launch went in the query %q", made[0].Query)
	}
}

func TestAWarmMachineIsStartingUntilItsImageIsReadyAndWarmOnceStoppedWithoutALogRead(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, machinesPath, answer(200, machines(
		warmMachineJSON("1", "hl-agent-m-pool-k", "created"),
		warmMachineJSON("2", "hl-agent-m-pool-k", "stopped"),
		warmMachineJSON("3", "hl-agent-m-pool-k", "failed"),
		warmMachineJSON("4", "hl-agent-m-pool-k", "started"),
	)))
	p := newTestProvider(t, api)

	listed, err := p.List(context.Background(), "hl-agent-m-")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]provider.State{}
	for _, i := range listed {
		got[i.ID] = i.State
	}
	want := map[string]provider.State{"1": provider.StateStarting, "2": provider.StateWarm, "3": provider.StateExited, "4": provider.StateRunning}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	if reads := api.sent(http.MethodGet, logsPath); len(reads) != 0 {
		t.Errorf("the log was read %d times for warm machines", len(reads))
	}
}

func TestStartWarmGivesTheMachineTheConfigACreateWouldHaveWaitsForTheReplacementAndStartsIt(t *testing.T) {
	api := newFakeAPI(t)
	machinePath := machinesPath + "/9001"
	var order []string
	note := func(step string, h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { order = append(order, step); h(w, r) }
	}
	api.on(http.MethodPost, machinePath, note("update", answer(200, `{"id":"9001","state":"created","instance_id":"01NEW"}`)))
	api.on(http.MethodGet, machinePath+"/wait", note("wait", answer(200, `{"ok":true}`)))
	api.on(http.MethodPost, machinePath+"/start", note("start", answer(200, `{"previous_state":"stopped"}`)))
	api.on(http.MethodGet, machinesPath, answer(200, machines(machineJSON("9001", "hl-agent-pe1-a2", "started", 0))))
	p := newTestProvider(t, api)
	spec := agentSpec()

	if err := p.StartWarm(context.Background(), "9001", spec); err != nil {
		t.Fatalf("StartWarm: %v", err)
	}
	if !reflect.DeepEqual(order, []string{"update", "wait", "start"}) {
		t.Fatalf("calls = %v, want update, wait for the replacement, start", order)
	}
	create, err := p.newCreateRequest(spec)
	if err != nil {
		t.Fatal(err)
	}
	create.SkipLaunch = true
	encoded, _ := json.Marshal(create)
	var want map[string]any
	_ = json.Unmarshal(encoded, &want)
	if update := api.sent(http.MethodPost, machinePath); !reflect.DeepEqual(update[0].Body, want) {
		t.Errorf("update body\n got %v\nwant %v", update[0].Body, want)
	}
	if wait := api.sent(http.MethodGet, machinePath+"/wait"); wait[0].Query != "state=stopped&timeout=25&instance_id=01NEW" {
		t.Errorf("wait query = %q", wait[0].Query)
	}
}

func TestStartWarmFailsWhenTheReplacementDoesNotSettleAndStartsNothing(t *testing.T) {
	api := newFakeAPI(t)
	machinePath := machinesPath + "/9001"
	api.on(http.MethodPost, machinePath, answer(200, `{"id":"9001","instance_id":"01NEW"}`))
	api.on(http.MethodGet, machinePath+"/wait", answer(408, `{"error":"deadline_exceeded"}`))
	p := newTestProvider(t, api)

	err := p.StartWarm(context.Background(), "9001", agentSpec())
	if err == nil || !strings.Contains(err.Error(), "did not settle") {
		t.Fatalf("StartWarm = %v, want the replacement named", err)
	}
	if starts := api.sent(http.MethodPost, machinePath+"/start"); len(starts) != 0 {
		t.Errorf("started %d times after the replacement failed", len(starts))
	}
}
