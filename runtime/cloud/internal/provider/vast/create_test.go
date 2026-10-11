package vast

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const (
	searchPath = "/api/v0/bundles/"
	listPath   = "/api/v1/instances/"
	rentPath   = "/api/v0/asks/7001/"
)

func gpuSpec() provider.Spec {
	return provider.Spec{
		Name:             "hl-exp1-a2",
		Image:            "registry.example/job:1",
		Command:          []string{"hl", "exec", "--stop-grace", "30", "--", "python", "train.py", "--note", `two  spaces 'and' "quotes" $HOME`, ""},
		Env:              map[string]string{"HYPOTHESISLOOP_TOKEN": "t = 'x' \"y\" $Z"},
		AcceleratorType:  "nvidia.com/gpu.product=NVIDIA-GeForce-RTX-4090",
		AcceleratorCount: 2,
		CPUMillis:        3500,
		MemoryBytes:      16<<30 + 1,
		DiskBytes:        40<<30 + 1,
	}
}

func cpuSpec() provider.Spec {
	spec := gpuSpec()
	spec.AcceleratorType, spec.AcceleratorCount = "", 0
	return spec
}

func gpuSettings() Settings {
	return Settings{
		MaxPricePerHour: 0.75,
		MinReliability:  0.98,
		Regions:         []string{"SE", "NL"},
		VerifiedOnly:    true,
		Accelerators: []Accelerator{
			{Product: "NVIDIA GeForce RTX 3060", GPUName: "RTX 3060"},
			{Product: "NVIDIA GeForce RTX 4090", GPUName: "RTX 4090", MinGPURAMMB: 24000},
		},
	}
}

// rentable wires the three calls of a Create that succeeds: one offer matches, the rent is
// accepted and the instance is listed straight after.
func rentable(api *fakeAPI, name string) {
	api.on(http.MethodGet, listPath, answer(200, instancesPage("", instanceJSON(9001, name, ""))))
	api.on(http.MethodPost, searchPath, answer(200, `{"offers":[{"id":7001,"machine_id":1,"dph_total":0.31}]}`))
	api.on(http.MethodPut, rentPath, answer(200, `{"success":true,"new_contract":9001,"instance_api_key":"never-read"}`))
}

func TestCreateSearchesForTheSpecAndRentsTheCheapestOffer(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	p := newTestProvider(t, api, gpuSettings())

	id, err := p.Create(context.Background(), gpuSpec())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "9001" {
		t.Fatalf("id = %q, want the contract id 9001", id)
	}

	search := api.sent(http.MethodPost, searchPath)
	if len(search) != 1 {
		t.Fatalf("searches = %d, want 1", len(search))
	}
	if search[0].Auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q", search[0].Auth)
	}
	wantSearch := map[string]any{
		"type":                "ondemand",
		"rentable":            map[string]any{"eq": true},
		"rented":              map[string]any{"eq": false},
		"num_gpus":            map[string]any{"eq": 2.0},
		"gpu_name":            map[string]any{"eq": "RTX 4090"},
		"gpu_ram":             map[string]any{"gte": 24000.0},
		"cpu_cores_effective": map[string]any{"gte": 4.0},
		"cpu_ram":             map[string]any{"gte": 16385.0},
		"disk_space":          map[string]any{"gte": 41.0},
		"allocated_storage":   41.0,
		"dph_total":           map[string]any{"lte": 0.75},
		"reliability2":        map[string]any{"gte": 0.98},
		"geolocation":         map[string]any{"in": []any{"SE", "NL"}},
		"verified":            map[string]any{"eq": true},
		"order":               []any{[]any{"dph_total", "asc"}},
		"limit":               25.0,
	}
	if !reflect.DeepEqual(search[0].Body, wantSearch) {
		t.Errorf("search body\n got %v\nwant %v", search[0].Body, wantSearch)
	}

	rent := api.sent(http.MethodPut, rentPath)
	if len(rent) != 1 {
		t.Fatalf("rents = %d, want 1", len(rent))
	}
	wantRent := map[string]any{
		"client_id": "me",
		"image":     "registry.example/job:1",
		"runtype":   "args",
		"label":     "hl-exp1-a2",
		"disk":      41.0,
		"env":       map[string]any{"HYPOTHESISLOOP_TOKEN": "t = 'x' \"y\" $Z"},
		"onstart":   "/bin/sh",
		"args": []any{"-c", supervisor(), "hl-supervisor",
			"hl", "exec", "--stop-grace", "30", "--", "python", "train.py", "--note", `two  spaces 'and' "quotes" $HOME`, ""},
	}
	if !reflect.DeepEqual(rent[0].Body, wantRent) {
		t.Errorf("rent body\n got %v\nwant %v", rent[0].Body, wantRent)
	}
}

// An image read only with a login is rented with it as vast.ai's image_login, the arguments of
// a docker login, and a public image with none.
func TestCreatePullsAnImageWithItsLoginAsImageLogin(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	p := newTestProvider(t, api, gpuSettings())
	spec := gpuSpec()
	spec.Image = "europe-west1-docker.pkg.dev/p/r-environments/environment@sha256:abc"
	spec.ImageLogin = &provider.ImageLogin{Registry: "europe-west1-docker.pkg.dev", Username: "oauth2accesstoken", Password: "pull-token"}

	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := api.sent(http.MethodPut, rentPath)[0].Body
	if got := body["image_login"]; got != "-u oauth2accesstoken -p pull-token europe-west1-docker.pkg.dev" {
		t.Fatalf("image_login = %v, want the login as docker login arguments", got)
	}
	if body["image"] != spec.Image {
		t.Fatalf("image = %v, want %s", body["image"], spec.Image)
	}
}

func TestCreateCPUOnlyTakesAnyGPUModelAndNoOptionalFilters(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	p := newTestProvider(t, api, Settings{MaxPricePerHour: 0.1})

	if _, err := p.Create(context.Background(), cpuSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := api.sent(http.MethodPost, searchPath)[0].Body
	if got := body["num_gpus"]; !reflect.DeepEqual(got, map[string]any{"gte": 1.0}) {
		t.Errorf("num_gpus = %v, want at least one: the vendor rents nothing without a GPU", got)
	}
	for _, absent := range []string{"gpu_name", "gpu_ram", "geolocation", "verified"} {
		if _, present := body[absent]; present {
			t.Errorf("search carries %s, which this spec and config do not ask for", absent)
		}
	}
}

func TestCreateSendsAnEmptyEnvAsAnObject(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	p := newTestProvider(t, api, Settings{})
	spec := cpuSpec()
	spec.Env = nil

	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if body := api.sent(http.MethodPut, rentPath)[0].Body; !reflect.DeepEqual(body["env"], map[string]any{}) {
		t.Errorf("env = %v, want an empty object", body["env"])
	}
}

func TestCreateMatchesTheAcceleratorTypeIgnoringCase(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	p := newTestProvider(t, api, gpuSettings())
	spec := gpuSpec()
	spec.AcceleratorType = "NVIDIA.com/gpu.product=nvidia-geforce-rtx-4090"

	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestCreateRefusesBeforeSpendingAnything(t *testing.T) {
	cases := map[string]struct {
		mutate func(*provider.Spec)
		want   string
	}{
		"no name":              {func(s *provider.Spec) { s.Name = "" }, "instance name"},
		"name outside the set": {func(s *provider.Spec) { s.Name = "HL job" }, "instance name"},
		"no command":           {func(s *provider.Spec) { s.Command = nil }, "no command"},
		"empty executable":     {func(s *provider.Spec) { s.Command = []string{"", "x"} }, "no command"},
		"no image":             {func(s *provider.Spec) { s.Image = "" }, "no image"},
		"no cpu":               {func(s *provider.Spec) { s.CPUMillis = 0 }, "cpu, memory and disk"},
		"no memory":            {func(s *provider.Spec) { s.MemoryBytes = 0 }, "cpu, memory and disk"},
		"no disk":              {func(s *provider.Spec) { s.DiskBytes = 0 }, "cpu, memory and disk"},
		"negative accelerator": {func(s *provider.Spec) { s.AcceleratorCount = -1 }, "-1 accelerators"},
		"port option as env":   {func(s *provider.Spec) { s.Env = map[string]string{"-p 22:22": "1"} }, "not an environment variable name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			p := newTestProvider(t, api, gpuSettings())
			spec := gpuSpec()
			tc.mutate(&spec)

			_, err := p.Create(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one about %q", err, tc.want)
			}
			if api.total() != 0 {
				t.Errorf("%d requests were sent for an invalid spec", api.total())
			}
		})
	}
}

func TestCreateRefusesAnAcceleratorTypeWithNoEntry(t *testing.T) {
	api := newFakeAPI(t)
	p := newTestProvider(t, api, gpuSettings())
	spec := gpuSpec()
	spec.AcceleratorType = "nvidia.com/gpu.product=NVIDIA-H200"

	_, err := p.Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "NVIDIA-H200") {
		t.Fatalf("err = %v, want one naming the type", err)
	}
	if api.total() != 0 {
		t.Errorf("%d requests were sent for a type the adapter cannot name", api.total())
	}
}

func TestCreateIsNotConfirmedByAnotherInstanceOfTheSameName(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	api.on(http.MethodGet, listPath, inSequence(
		answer(200, instancesPage("", instanceJSON(1, "hl-exp1-a2", "running"), instanceJSON(9001, "hl-exp1-a21", ""))),
		answer(200, instancesPage("", instanceJSON(1, "hl-exp1-a2", "running"), instanceJSON(9001, "hl-exp1-a2", ""))),
	))
	p := newTestProvider(t, api, gpuSettings())

	if id, err := p.Create(context.Background(), gpuSpec()); err != nil || id != "9001" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if got := len(api.sent(http.MethodGet, listPath)); got != 2 {
		t.Errorf("lists = %d, want 2: the first showed only the older instance of the name", got)
	}
}

func TestCreateFailsWhenNoOfferMatches(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, answer(200, `{"offers":[]}`))
	p := newTestProvider(t, api, gpuSettings())

	_, err := p.Create(context.Background(), gpuSpec())
	if err == nil || !strings.Contains(err.Error(), "no offer matches") {
		t.Fatalf("err = %v, want no offer matches", err)
	}
	if api.total() != 1 {
		t.Errorf("%d requests, want the one search", api.total())
	}
}

// taken is the vendor's answer for an offer its search still returned after it was rented. A real
// account answered with HTTP 400; the same body inside a 200 is treated alike.
const taken = `{"success":false,"error":"invalid_args","msg":"error 404/3603: no_such_ask  Instance type by id 7001 is not available.","ask_id":7001}`

func TestCreatePassesOverAnOfferTakenBetweenSearchAndRent(t *testing.T) {
	for name, status := range map[string]int{"as an HTTP error": 400, "inside a 200": 200} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			rentable(api, "hl-exp1-a2")
			api.on(http.MethodPost, searchPath, answer(200, `{"offers":[{"id":7001,"machine_id":1,"dph_total":0.31},{"id":7002,"machine_id":2,"dph_total":0.32},{"id":7003,"machine_id":3,"dph_total":0.33}]}`))
			api.on(http.MethodPut, rentPath, answer(status, taken))
			api.on(http.MethodPut, "/api/v0/asks/7002/", answer(200, `{"success":true,"new_contract":9001}`))
			p := newTestProvider(t, api, gpuSettings())

			id, err := p.Create(context.Background(), gpuSpec())
			if err != nil || id != "9001" {
				t.Fatalf("Create = %q, %v, want the instance rented from the second offer", id, err)
			}
			if got := len(api.sent(http.MethodPost, searchPath)); got != 1 {
				t.Errorf("searches = %d, want 1", got)
			}
		})
	}
}

// conflicting is the vendor's answer for an offer it still lists while its GPU is rented, as it
// answered for the offers this account's own instances held.
const conflicting = `{"success":false,"error":"invalid_args","msg":"error 400/3763: GPU conflict: requesting already rented gpus {59011}"}`

func TestCreateFindsNoOfferWhenEveryOfferFoundWasTaken(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, answer(200, `{"offers":[{"id":7001,"machine_id":1,"dph_total":0.31},{"id":7002,"machine_id":2,"dph_total":0.32}]}`))
	api.on(http.MethodPut, rentPath, answer(400, taken))
	api.on(http.MethodPut, "/api/v0/asks/7002/", answer(400, conflicting))
	p := newTestProvider(t, api, gpuSettings())

	_, err := p.Create(context.Background(), gpuSpec())
	if !errors.Is(err, provider.ErrNoOffer) || !strings.Contains(err.Error(), "every one of the 2 offers tried") || !strings.Contains(err.Error(), "already rented") {
		t.Fatalf("err = %v, want no offer, every one taken, with the vendor's reason", err)
	}
	if api.total() != 3 {
		t.Errorf("requests = %d, want the search and two refused rents: nothing was rented, nothing is left", api.total())
	}
}

// The search goes on listing an offer this account has rented. Jobs rented one after another
// are each given an offer no instance of this process holds, and an offer is given out again
// once its instance is destroyed or gone from the account.
func TestCreatesOfOneProcessNeverAskForAnOfferItsOwnInstancesHold(t *testing.T) {
	api := newFakeAPI(t)
	ctx := context.Background()
	api.on(http.MethodPost, searchPath, offersOn(11, 12))
	api.on(http.MethodPut, rentPath, answer(200, `{"success":true,"new_contract":9001}`))
	api.on(http.MethodPut, "/api/v0/asks/7002/", answer(200, `{"success":true,"new_contract":9002}`))
	api.on(http.MethodDelete, "/api/v0/instances/9001/", answer(200, `{"success":true}`))
	both := instancesPage("", instanceJSON(9001, "hl-exp1-a2", ""), instanceJSON(9002, "hl-exp2-a0", ""))
	api.on(http.MethodGet, listPath, answer(200, both))
	p := newTestProvider(t, api, gpuSettings())
	create := func(name string) (string, error) {
		spec := gpuSpec()
		spec.Name = name
		return p.Create(ctx, spec)
	}
	asked := func(offer string) int { return len(api.sent(http.MethodPut, "/api/v0/asks/"+offer+"/")) }

	first, firstErr := create("hl-exp1-a2")
	second, secondErr := create("hl-exp2-a0")
	if firstErr != nil || secondErr != nil || first != "9001" || second != "9002" || asked("7001") != 1 {
		t.Fatalf("rented %q %v and %q %v asking for the held offer %d times, want one instance from each offer", first, firstErr, second, secondErr, asked("7001"))
	}
	if _, err := create("hl-exp3-a0"); !errors.Is(err, provider.ErrNoOffer) || asked("7001")+asked("7002") != 2 {
		t.Fatalf("a third job: err = %v after %d rents, want no offer and nothing asked of the vendor", err, asked("7001")+asked("7002"))
	}

	if err := p.Destroy(ctx, first); err != nil {
		t.Fatal(err)
	}
	api.on(http.MethodGet, listPath, answer(200, instancesPage("", instanceJSON(9001, "hl-exp3-a0", ""))))
	if _, err := create("hl-exp3-a0"); err != nil || asked("7001") != 2 {
		t.Fatalf("after the destroy: %v, offer 7001 asked for %d times, want it rented again", err, asked("7001"))
	}
	// Instance 9002 left the account without a Destroy, and the read above showed it gone.
	api.on(http.MethodGet, listPath, answer(200, both))
	if _, err := create("hl-exp2-a0"); err != nil || asked("7002") != 2 {
		t.Fatalf("after its instance left the account: %v, offer 7002 asked for %d times, want it rented again", err, asked("7002"))
	}
}

func TestCreateStopsAtARefusalThatIsNotATakenOffer(t *testing.T) {
	refusals := map[string]http.HandlerFunc{
		"no credit":        answer(400, `{"success":false,"error":"insufficient_credit","msg":"Your account lacks credit."}`),
		"refusal in a 200": answer(200, `{"success":false,"msg":"image not allowed"}`),
		"no contract id":   answer(200, `{"success":true}`),
	}
	for name, refusal := range refusals {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPost, searchPath, answer(200, `{"offers":[{"id":7001,"machine_id":1,"dph_total":0.31},{"id":7002,"machine_id":2,"dph_total":0.32}]}`))
			api.on(http.MethodPut, rentPath, refusal)
			p := newTestProvider(t, api, gpuSettings())

			if _, err := p.Create(context.Background(), gpuSpec()); err == nil {
				t.Fatal("no error")
			}
			if api.total() != 2 {
				t.Errorf("requests = %d, want the search and the one refused rent", api.total())
			}
		})
	}
}

func TestCreateReturnsOnlyOnceTheInstanceIsListed(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	api.on(http.MethodGet, listPath, inSequence(
		answer(200, instancesPage("")),
		answer(200, instancesPage("")),
		answer(200, instancesPage("", instanceJSON(9001, "hl-exp1-a2", ""))),
	))
	p := newTestProvider(t, api, gpuSettings())

	if _, err := p.Create(context.Background(), gpuSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := len(api.sent(http.MethodGet, listPath)); got != 3 {
		t.Errorf("lists = %d, want 3", got)
	}
}

// A Create that fails after renting must leave nothing a caller cannot find, so it gives the
// instance back: by a deadline that has already ended the wait, or by a List that fails.
func TestCreateDestroysAnInstanceItCannotConfirm(t *testing.T) {
	unconfirmed := map[string]http.HandlerFunc{
		"never listed": answer(200, instancesPage("")),
		"list failing": answer(503, `unavailable`),
		"rate limited": answer(429, `API requests too frequent: endpoint threshold=1.0`),
	}
	for name, list := range unconfirmed {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			rentable(api, "hl-exp1-a2")
			api.on(http.MethodGet, listPath, list)
			api.on(http.MethodDelete, "/api/v0/instances/9001/", answer(200, `{"success":true}`))
			p := newTestProvider(t, api, gpuSettings())
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			_, err := p.Create(ctx, gpuSpec())
			if err == nil || !strings.Contains(err.Error(), "9001") || !strings.Contains(err.Error(), "it was destroyed") {
				t.Fatalf("err = %v, want one naming instance 9001 and that it was destroyed", err)
			}
			if got := len(api.sent(http.MethodDelete, "/api/v0/instances/9001/")); got != 1 {
				t.Errorf("destroys = %d, want 1", got)
			}
		})
	}
}

func TestCreateSaysSoWhenItCannotGiveTheInstanceBack(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	api.on(http.MethodGet, listPath, answer(503, `unavailable`))
	api.on(http.MethodDelete, "/api/v0/instances/9001/", answer(503, `unavailable`))
	p := newTestProvider(t, api, gpuSettings())

	_, err := p.Create(context.Background(), gpuSpec())
	if err == nil || !strings.Contains(err.Error(), "9001") || !strings.Contains(err.Error(), "may still be rented") {
		t.Fatalf("err = %v, want one naming instance 9001 as possibly still rented", err)
	}
}

func TestCreateDoesNotRetryARateLimitOrAServerError(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		for _, failing := range []string{"search", "rent"} {
			t.Run(failing+" "+http.StatusText(status), func(t *testing.T) {
				api := newFakeAPI(t)
				rentable(api, "hl-exp1-a2")
				route := map[string][2]string{
					"search": {http.MethodPost, searchPath},
					"rent":   {http.MethodPut, rentPath},
				}[failing]
				api.on(route[0], route[1], answer(status, `{"detail":"slow down"}`))
				p := newTestProvider(t, api, gpuSettings())

				_, err := p.Create(context.Background(), gpuSpec())
				if err == nil || !strings.Contains(err.Error(), "HTTP "+jsonInt(status)) {
					t.Fatalf("err = %v, want one carrying HTTP %d", err, status)
				}
				if got := len(api.sent(route[0], route[1])); got != 1 {
					t.Errorf("%s requests = %d, want 1", failing, got)
				}
			})
		}
	}
}

// offersOn answers a search with one offer per entry, cheapest first: offer 7001 on the first
// machine named, 7002 on the second, and so on.
func offersOn(machines ...int) http.HandlerFunc {
	var offers []string
	for i, machine := range machines {
		offers = append(offers, fmt.Sprintf(`{"id":%d,"machine_id":%d,"dph_total":%.2f}`, 7001+i, machine, 0.30+float64(i)/100))
	}
	return answer(200, `{"offers":[`+strings.Join(offers, ",")+`]}`)
}

func rentedOffer(t *testing.T, api *fakeAPI) string {
	t.Helper()
	for _, sent := range api.requests {
		if sent.Method == http.MethodPut && strings.HasPrefix(sent.Path, "/api/v0/asks/") {
			return strings.Trim(strings.TrimPrefix(sent.Path, "/api/v0/asks/"), "/")
		}
	}
	t.Fatal("nothing was rented")
	return ""
}

// A machine that cannot start a container stays the cheapest offer, so a Create that always took
// the cheapest sent every retry of a job back to it.
func TestCreateSpreadsItsChoiceOverTheCheapestMachines(t *testing.T) {
	rentedFrom := map[string]int{}
	for range 200 {
		api := newFakeAPI(t)
		api.on(http.MethodGet, listPath, answer(200, instancesPage("", instanceJSON(9001, "hl-exp1-a2", ""))))
		api.on(http.MethodPost, searchPath, offersOn(11, 12, 13, 14, 15, 16, 17))
		for offer := 7001; offer <= 7007; offer++ {
			api.on(http.MethodPut, fmt.Sprintf("/api/v0/asks/%d/", offer), answer(200, `{"success":true,"new_contract":9001}`))
		}
		p := newTestProvider(t, api, gpuSettings())
		p.shuffle = shuffleOffers

		if _, err := p.Create(context.Background(), gpuSpec()); err != nil {
			t.Fatalf("Create: %v", err)
		}
		rentedFrom[rentedOffer(t, api)]++
	}
	for _, offer := range []string{"7001", "7002", "7003", "7004", "7005"} {
		if rentedFrom[offer] == 0 {
			t.Errorf("200 rentals never took offer %s, one of the five cheapest machines: %v", offer, rentedFrom)
		}
	}
	if rentedFrom["7006"]+rentedFrom["7007"] != 0 {
		t.Errorf("rentals reached past the five cheapest machines: %v", rentedFrom)
	}
	if rentedFrom["7001"] > 120 {
		t.Errorf("the cheapest machine was taken %d times of 200, which a broken one would be", rentedFrom["7001"])
	}
}

func TestCreateCountsAMachineOnceHoweverManyOffersItHas(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	api.on(http.MethodPost, searchPath, offersOn(11, 11, 11, 12, 11, 13, 14, 15, 16))
	api.on(http.MethodPut, "/api/v0/asks/7008/", answer(200, `{"success":true,"new_contract":9001}`))
	p := newTestProvider(t, api, gpuSettings())
	// The last candidate first: the fifth machine, which is the eighth offer.
	p.shuffle = func(offers []offer) { slices.Reverse(offers) }

	if _, err := p.Create(context.Background(), gpuSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := rentedOffer(t, api); got != "7008" {
		t.Errorf("rented offer %s, want 7008: machine 11 fills four of the nine offers and counts once", got)
	}
}

func TestCreateNeverSearchesAnExcludedMachine(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	settings := gpuSettings()
	settings.ExcludedMachines = []int64{124592, 31}
	p := newTestProvider(t, api, settings)

	if _, err := p.Create(context.Background(), gpuSpec()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := api.sent(http.MethodPost, searchPath)[0].Body
	if got := body["machine_id"]; !reflect.DeepEqual(got, map[string]any{"notin": []any{124592.0, 31.0}}) {
		t.Errorf("machine_id filter = %v, want both machines excluded", got)
	}
}

// A host that could not start one instance is the cheapest again the moment that instance is
// destroyed. The caller names the hosts it saw fail, as List named them, and the search leaves
// them out beside the ones the settings exclude.
func TestCreateNeverSearchesAHostTheSpecAvoids(t *testing.T) {
	api := newFakeAPI(t)
	rentable(api, "hl-exp1-a2")
	settings := gpuSettings()
	settings.ExcludedMachines = []int64{31}
	p := newTestProvider(t, api, settings)
	spec := gpuSpec()
	spec.AvoidHosts = []string{"124592", "77"}

	if _, err := p.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	body := api.sent(http.MethodPost, searchPath)[0].Body
	if got := body["machine_id"]; !reflect.DeepEqual(got, map[string]any{"notin": []any{31.0, 124592.0, 77.0}}) {
		t.Errorf("machine_id filter = %v, want the excluded machine and both avoided hosts", got)
	}
	if !reflect.DeepEqual(settings.ExcludedMachines, []int64{31}) {
		t.Errorf("the settings' excluded machines became %v", settings.ExcludedMachines)
	}

	spec.AvoidHosts = []string{"fly-host-a"}
	before := api.total()
	if _, err := p.Create(context.Background(), spec); err == nil || !strings.Contains(err.Error(), `avoids host "fly-host-a"`) {
		t.Fatalf("Create avoiding a host that is no machine id: err = %v, want a refusal", err)
	}
	if api.total() != before {
		t.Errorf("%d requests were sent for a spec the adapter cannot meet", api.total()-before)
	}
}
