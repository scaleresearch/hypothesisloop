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

func agentSpec() provider.Spec {
	return provider.Spec{
		Name:         "hl-agent-pe1-a2",
		Image:        "registry.example/agent:1",
		Architecture: "amd64",
		Command:      []string{"uv", "run", "agent", "--note", `two  spaces 'and' "quotes" $HOME`, ""},
		Env:          map[string]string{"HL_TOKEN": "t = 'x' \"y\" $Z"},
		CPUMillis:    2000,
		MemoryBytes:  4 << 30,
		DiskBytes:    10<<30 + 1,
	}
}

// creatable wires the two calls of a Create that succeeds: the Machine is made and is listed
// straight after.
func creatable(api *fakeAPI, name string) {
	api.on(http.MethodPost, machinesPath, answer(200, `{"id":"9001","state":"created"}`))
	api.on(http.MethodGet, machinesPath, answer(200, machines(machineJSON("9001", name, "created", 0))))
}

func TestCreateMakesAMachineThatRunsTheCommandOnceAndStaysWhenItEnds(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	p := newTestProvider(t, api)

	id, err := p.Create(context.Background(), agentSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "9001" {
		t.Fatalf("id = %q, want the machine id 9001", id)
	}
	made := api.sent(http.MethodPost, machinesPath)
	if len(made) != 1 {
		t.Fatalf("creates = %d, want 1", len(made))
	}
	if made[0].Auth != testToken {
		t.Errorf("Authorization = %q, want the token as it is, scheme included", made[0].Auth)
	}
	want := map[string]any{
		"region": "ams",
		"config": map[string]any{
			"image": "registry.example/agent:1",
			"init": map[string]any{"exec": []any{"/bin/sh", "-c", supervisor(), "hl-supervisor",
				"uv", "run", "agent", "--note", `two  spaces 'and' "quotes" $HOME`, ""}},
			"env":          map[string]any{"HL_TOKEN": "t = 'x' \"y\" $Z"},
			"metadata":     map[string]any{"hl_name": "hl-agent-pe1-a2"},
			"guest":        map[string]any{"cpu_kind": "shared", "cpus": 2.0, "memory_mb": 4096.0},
			"rootfs":       map[string]any{"size_gb": 11.0},
			"restart":      map[string]any{"policy": "no"},
			"auto_destroy": false,
		},
	}
	if !reflect.DeepEqual(made[0].Body, want) {
		t.Errorf("create body\n got %v\nwant %v", made[0].Body, want)
	}
}

func TestCreateSendsAnEmptyEnvAsAnObject(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	p := newTestProvider(t, api)
	spec := agentSpec()
	spec.Env = nil

	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	config := api.sent(http.MethodPost, machinesPath)[0].Body["config"].(map[string]any)
	if !reflect.DeepEqual(config["env"], map[string]any{}) {
		t.Errorf("env = %v, want an empty object", config["env"])
	}
}

func TestCreateRefusesBeforeSpendingAnything(t *testing.T) {
	cases := map[string]struct {
		mutate func(*provider.Spec)
		want   string
	}{
		"no name":              {func(s *provider.Spec) { s.Name = "" }, "instance name"},
		"name outside the set": {func(s *provider.Spec) { s.Name = "HL agent" }, "instance name"},
		"no command":           {func(s *provider.Spec) { s.Command = nil }, "no command"},
		"empty executable":     {func(s *provider.Spec) { s.Command = []string{"", "x"} }, "no command"},
		"no image":             {func(s *provider.Spec) { s.Image = "" }, "no image"},
		"no cpu":               {func(s *provider.Spec) { s.CPUMillis = 0 }, "cpu, memory and disk"},
		"no memory":            {func(s *provider.Spec) { s.MemoryBytes = 0 }, "cpu, memory and disk"},
		"no disk":              {func(s *provider.Spec) { s.DiskBytes = 0 }, "cpu, memory and disk"},
		"an accelerator": {func(s *provider.Spec) {
			s.AcceleratorCount, s.AcceleratorType = 1, "nvidia.com/gpu.product=NVIDIA-H100"
		}, "rents none"},
		"negative accelerator": {func(s *provider.Spec) { s.AcceleratorCount = -1 }, "-1 accelerators"},
		"image login": {func(s *provider.Spec) {
			s.ImageLogin = &provider.ImageLogin{Registry: "r", Username: "u", Password: "p"}
		}, "only public images"},
		"not a variable name":  {func(s *provider.Spec) { s.Env = map[string]string{"A B": "1"} }, "not an environment variable name"},
		"more cores than sold": {func(s *provider.Spec) { s.CPUMillis = 9000 }, "8 cores"},
		"more disk than held":  {func(s *provider.Spec) { s.DiskBytes = 51 << 30 }, "at most 50 GB"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			p := newTestProvider(t, api)
			spec := agentSpec()
			tc.mutate(&spec)

			_, err := p.Create(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one about %q", err, tc.want)
			}
			if api.total() != 0 {
				t.Errorf("%d requests were sent for a spec the adapter cannot meet", api.total())
			}
		})
	}
}

// The API pulls the image before it answers a create, so an image that cannot be pulled, a
// region with no capacity and a rate limit are all an answer to the one request.
func TestCreateReportsARefusalAndLeavesNothing(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"image that cannot be pulled": {400, `{"error":"failed to get manifest registry.example/agent:1: request failed: not found [http 404]"}`},
		"no capacity":                 {412, `{"error":"insufficient resources to create new machine with existing volume"}`},
		"rate limited":                {429, `{"error":"rate limit exceeded"}`},
		"server error":                {503, `unavailable`},
		"app that does not exist":     {404, `{"error":"app not found"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPost, machinesPath, answer(tc.status, tc.body))
			p := newTestProvider(t, api)

			_, err := p.Create(context.Background(), agentSpec())
			if err == nil || !strings.Contains(err.Error(), tc.body) {
				t.Fatalf("err = %v, want one carrying the vendor's answer", err)
			}
			if api.total() != 1 {
				t.Errorf("requests = %d, want the one create and no retry", api.total())
			}
		})
	}
}

func TestCreateWithoutAnIDIsAnError(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, machinesPath, answer(200, `{}`))
	p := newTestProvider(t, api)

	if _, err := p.Create(context.Background(), agentSpec()); err == nil || !strings.Contains(err.Error(), "without an id") {
		t.Fatalf("err = %v, want one about the missing id", err)
	}
}

func TestCreateIsNotConfirmedByAnotherMachineOfTheSameName(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodGet, machinesPath, inSequence(
		answer(200, machines(machineJSON("1", "hl-agent-pe1-a2", "started", 0))),
		answer(200, machines(machineJSON("1", "hl-agent-pe1-a2", "started", 0), machineJSON("9001", "hl-agent-pe1-a2", "created", 0))),
	))
	p := newTestProvider(t, api)

	if id, err := p.Create(context.Background(), agentSpec()); err != nil || id != "9001" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if got := len(api.sent(http.MethodGet, machinesPath)); got != 2 {
		t.Errorf("lists = %d, want 2: the first showed only the older machine of the name", got)
	}
}

func TestCreateReadsTheListAgainAfterAListThatFailed(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"rate limited": answer(429, `{"error":"rate limit exceeded"}`),
		"server error": answer(500, `oops`),
	}
	for name, refusal := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			creatable(api, "hl-agent-pe1-a2")
			api.on(http.MethodGet, machinesPath, inSequence(refusal, refusal,
				answer(200, machines(machineJSON("9001", "hl-agent-pe1-a2", "created", 0)))))
			p := newTestProvider(t, api)

			if id, err := p.Create(context.Background(), agentSpec()); err != nil || id != "9001" {
				t.Fatalf("Create = %q, %v, want the machine: one refused list says nothing about it", id, err)
			}
			if got := len(api.sent(http.MethodDelete, machinesPath+"/9001")); got != 0 {
				t.Errorf("destroys = %d: a machine that exists was destroyed over a list that failed", got)
			}
		})
	}
}

func TestCreateDestroysAMachineItCouldNotConfirmInTheWholeWindow(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"the list fails":        answer(500, `oops`),
		"the list is rate held": answer(429, `{"error":"rate limit exceeded"}`),
	}
	for name, list := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			creatable(api, "hl-agent-pe1-a2")
			api.on(http.MethodGet, machinesPath, list)
			api.on(http.MethodDelete, machinesPath+"/9001", answer(200, `{"ok":true}`))
			p := newTestProvider(t, api)

			_, err := p.Create(context.Background(), agentSpec())
			if err == nil || !strings.Contains(err.Error(), "could not be confirmed in the list") || !strings.Contains(err.Error(), "it was destroyed") {
				t.Fatalf("err = %v, want one saying the machine could not be confirmed and was destroyed", err)
			}
			if got := len(api.sent(http.MethodGet, machinesPath)); got < 3 {
				t.Errorf("lists = %d, want the list read again for the whole window", got)
			}
			if got := api.sent(http.MethodDelete, machinesPath+"/9001"); len(got) != 1 || got[0].Query != "force=true" {
				t.Errorf("destroys = %+v, want one, forced", got)
			}
		})
	}
}

func TestCreateSaysWhenTheMachineItCannotConfirmMayStillBeRented(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodGet, machinesPath, answer(500, `oops`))
	api.on(http.MethodDelete, machinesPath+"/9001", answer(500, `oops`))
	p := newTestProvider(t, api)

	_, err := p.Create(context.Background(), agentSpec())
	if err == nil || !strings.Contains(err.Error(), "may still be rented") || !strings.Contains(err.Error(), "9001") {
		t.Fatalf("err = %v, want one naming the machine that may still be rented", err)
	}
}

func TestCreateGivesUpOnAMachineThatNeverAppearsInTheList(t *testing.T) {
	api := newFakeAPI(t)
	creatable(api, "hl-agent-pe1-a2")
	api.on(http.MethodGet, machinesPath, answer(200, machines()))
	api.on(http.MethodDelete, machinesPath+"/9001", answer(200, `{"ok":true}`))
	p := newTestProvider(t, api)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := p.Create(ctx, agentSpec())
	if err == nil || !strings.Contains(err.Error(), "is not in the list") {
		t.Fatalf("err = %v, want one saying the machine is not in the list", err)
	}
	if got := len(api.sent(http.MethodDelete, machinesPath+"/9001")); got != 1 {
		t.Errorf("destroys = %d, want 1: the caller's context ended and the machine must still go", got)
	}
}
