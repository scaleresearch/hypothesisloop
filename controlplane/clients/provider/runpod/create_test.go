package runpod

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

// created answers a create with the pod and lists it from then on.
func created(api *fakeAPI, pod string) {
	api.on(http.MethodPost, "/v2/pods", reply(201, pod))
	api.on(http.MethodGet, "/v2/pods", reply(200, podsPage(pod)))
}

func TestCreateRentsTheCheapestCPUFlavorWithStockAndHandsTheArgvOverUnparsed(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	api.on(http.MethodGet, "/v2/catalog/cpus", reply(200, cpusJSON))
	created(api, podJSON("p1", "hl-job-a", "RUNNING"))
	spec := cpuSpec(3, 8)
	spec.Command = []string{"python", "-c", "print('a  b')", "", `$HOME "x"`}
	spec.Env = map[string]string{"HL_TOKEN": "t"}

	id, err := p.Create(context.Background(), spec)
	if err != nil || id != "p1" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	body := api.sent(http.MethodPost, "/v2/pods")[0].Body
	if body["name"] != "hl-job-a" || body["image"] != spec.Image || body["cloud"] != "SECURE" || body["disk"] != float64(8) || body["globalNetworking"] != nil || body["gpu"] != nil {
		t.Errorf("body %+v", body)
	}
	if cpu := body["cpu"].(map[string]any); cpu["id"] != "cpu3c" || cpu["vcpuCount"] != float64(4) {
		t.Errorf("cpu %+v, want the cheapest flavor at the power of two that holds 3 cores and 8 GiB", cpu)
	}
	if dcs := body["dataCenterIds"]; !reflect.DeepEqual(dcs, []any{"EU-SE-1", "US-CA-2"}) {
		t.Errorf("data centers %v, want the most stocked first", dcs)
	}
	if env := body["env"].(map[string]any); env["HL_TOKEN"] != "t" {
		t.Errorf("env %+v", env)
	}
	argv := append(toStrings(body["entrypoint"]), toStrings(body["cmd"])...)
	if want := supervisedCommand(spec.Name, spec.Command, false); !reflect.DeepEqual(argv, want) {
		t.Errorf("argv %q, want %q", argv, want)
	}
	if got := argv[len(argv)-len(spec.Command):]; !reflect.DeepEqual(got, spec.Command) {
		t.Errorf("the command reaches the supervisor as %q", got)
	}
}

func toStrings(value any) []string {
	var out []string
	for _, v := range value.([]any) {
		out = append(out, v.(string))
	}
	return out
}

func TestCreateTriesTheNextFlavorWhenTheVendorHasNoMachineAndNoOtherRefusal(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	ctx := context.Background()
	api.on(http.MethodGet, "/v2/catalog/cpus", reply(200, cpusJSON))
	full := problem(400, "There are no longer any instances available with the requested specifications. Please refresh and try again.")
	api.on(http.MethodPost, "/v2/pods", full)
	if _, err := p.Create(ctx, cpuSpec(2, 4)); !errors.Is(err, provider.ErrNoOffer) {
		t.Fatalf("no machine for any flavor: %v, want ErrNoOffer", err)
	}
	asked := api.sent(http.MethodPost, "/v2/pods")
	if len(asked) != 2 || asked[0].Body["cpu"].(map[string]any)["id"] != "cpu3c" || asked[1].Body["cpu"].(map[string]any)["id"] != "cpu5c" {
		t.Fatalf("creates %+v, want each flavor with stock once, the cheapest first", asked)
	}

	api.on(http.MethodPost, "/v2/pods", problem(400, "env: too many variables"))
	if _, err := p.Create(ctx, cpuSpec(2, 4)); err == nil || errors.Is(err, provider.ErrNoOffer) || !strings.Contains(err.Error(), "too many variables") {
		t.Fatalf("a request the vendor refuses: %v, want the refusal and no ErrNoOffer", err)
	}
	api.on(http.MethodPost, "/v2/pods", problem(402, "insufficient balance"))
	if _, err := p.Create(ctx, cpuSpec(2, 4)); err == nil || errors.Is(err, provider.ErrNoOffer) {
		t.Fatalf("no balance: %v, want an error that is no ErrNoOffer", err)
	}
}

func TestCreateOfAGPUPodInAPlaceJoinsTheGlobalNetworkThereAndWaitsForItsAddress(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	api.on(http.MethodGet, "/v2/catalog/gpus/NVIDIA RTX A4500", reply(200, gpuJSON(`{"id":"EU-SE-1","availability":"HIGH"},{"id":"EU-RO-1","availability":"LOW"}`)))
	api.on(http.MethodPost, "/v2/pods", reply(201, podJSON("p1", "hl-job-g", "RUNNING")))
	addressed := strings.Replace(podJSON("p1", "hl-job-g", "RUNNING"), `"globalNetworking":{"enabled":false}`, `"globalNetworking":{"enabled":true,"ip":"10.0.1.2"}`, 1)
	lists := 0
	api.on(http.MethodGet, "/v2/pods", func(w http.ResponseWriter, r *http.Request) {
		lists++
		if lists < 3 {
			reply(200, podsPage(podJSON("p1", "hl-job-g", "RUNNING")))(w, r)
			return
		}
		reply(200, podsPage(addressed))(w, r)
	})
	spec := gpuSpec("hl-job-g")
	spec.Place = "EU-RO-1"
	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if lists < 3 {
		t.Errorf("Create returned before the pod showed its address")
	}
	body := api.sent(http.MethodPost, "/v2/pods")[0].Body
	gpu := body["gpu"].(map[string]any)
	if body["globalNetworking"] != true || !reflect.DeepEqual(body["dataCenterIds"], []any{"EU-RO-1"}) || body["cpu"] != nil {
		t.Errorf("body %+v", body)
	}
	if gpu["id"] != "NVIDIA RTX A4500" || gpu["count"] != float64(1) || gpu["minVcpuCountPerGpu"] != float64(8) || gpu["minRamPerGpu"] != float64(24) {
		t.Errorf("gpu %+v", gpu)
	}
	if argv := toStrings(body["cmd"]); !strings.Contains(argv[1], "/sys/class/net/"+interfaceName) {
		t.Errorf("a pod on the global network does not wait for its interface")
	}
}

func TestCreateGivesBackAPodThatNeverShowsInTheList(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	api.on(http.MethodGet, "/v2/catalog/cpus", reply(200, cpusJSON))
	api.on(http.MethodPost, "/v2/pods", reply(201, podJSON("p1", "hl-job-a", "RUNNING")))
	api.on(http.MethodGet, "/v2/pods", reply(200, podsPage()))
	api.on(http.MethodDelete, "/v2/pods/p1", reply(204, ""))
	ctx, cancel := context.WithTimeout(context.Background(), 50_000_000)
	defer cancel()
	if _, err := p.Create(ctx, cpuSpec(2, 4)); err == nil || !strings.Contains(err.Error(), "destroyed") {
		t.Fatalf("err = %v, want the pod given back", err)
	}
	if len(api.sent(http.MethodDelete, "/v2/pods/p1")) != 1 {
		t.Fatal("the pod was not destroyed")
	}
}

func TestCreateRefusesWhatTheAdapterCannotRent(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, testSettings())
	cases := map[string]func(*provider.Spec){
		"another architecture": func(s *provider.Spec) { s.Architecture = "arm64" },
		"a registry login": func(s *provider.Spec) {
			s.ImageLogin = &provider.ImageLogin{Registry: "r", Username: "u", Password: "p"}
		},
		"hosts to avoid": func(s *provider.Spec) { s.AvoidHosts = []string{"h"} },
		"no command":     func(s *provider.Spec) { s.Command = nil },
	}
	for name, mutate := range cases {
		spec := cpuSpec(2, 4)
		mutate(&spec)
		if _, err := p.Create(context.Background(), spec); err == nil {
			t.Errorf("%s: Create accepted it", name)
		}
	}
	cpuInPlace := cpuSpec(2, 4)
	cpuInPlace.Place = "EU-RO-1"
	api.on(http.MethodGet, "/v2/catalog/cpus", reply(200, cpusJSON))
	if _, err := p.Create(context.Background(), cpuInPlace); !errors.Is(err, provider.ErrNoOffer) || !strings.Contains(err.Error(), "GPU pods only") {
		t.Errorf("a CPU pod on the global network: %v", err)
	}
	if len(api.sent(http.MethodPost, "/v2/pods")) != 0 {
		t.Error("a refused spec was sent to the vendor")
	}
}
