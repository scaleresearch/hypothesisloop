package vast

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider"
)

func TestRentableCountsTheOffersCreateWouldChooseAmong(t *testing.T) {
	api := newFakeAPI(t)
	ctx := context.Background()
	api.on(http.MethodPost, searchPath, offersOn(11, 12, 13))
	api.on(http.MethodPut, rentPath, answer(200, `{"success":true,"new_contract":9001}`))
	api.on(http.MethodGet, listPath, answer(200, instancesPage("", instanceJSON(9001, "hl-exp1-a2", ""))))
	p := newTestProvider(t, api, gpuSettings())

	if supply, err := p.Rentable(ctx, gpuSpec(), 200); err != nil || supply != (provider.Supply{Count: 3}) {
		t.Fatalf("Rentable = %+v, %v, want the 3 offers within the filters", supply, err)
	}
	if supply, err := p.Rentable(ctx, gpuSpec(), 2); err != nil || supply.Count != 2 {
		t.Fatalf("Rentable = %+v, %v, want no more than asked about", supply, err)
	}
	searches := api.sent(http.MethodPost, searchPath)
	create := gpuSpec()
	if _, err := p.Create(ctx, create); err != nil {
		t.Fatal(err)
	}
	rent := api.sent(http.MethodPost, searchPath)[len(searches)].Body
	for key, want := range rent {
		if key == "limit" {
			continue
		}
		if got := searches[0].Body[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("Rentable searched %s = %v, and Create rents within %v", key, got, want)
		}
	}
	if limit := searches[0].Body["limit"]; limit != float64(200) {
		t.Errorf("Rentable asked for %v offers, want 200", limit)
	}

	// The search goes on listing the offer this process now holds: it is not there to rent.
	if supply, err := p.Rentable(ctx, gpuSpec(), 200); err != nil || supply.Count != 2 {
		t.Fatalf("Rentable with one offer held = %+v, %v, want 2", supply, err)
	}
	last := api.sent(http.MethodPost, searchPath)
	if limit := last[len(last)-1].Body["limit"]; limit != float64(201) {
		t.Errorf("Rentable asked for %v offers with one held, want 201", limit)
	}
}

// No offer passes the filters, and there are offers of the size: the answer names the filters
// that leave them out, with no figures, and the detail counts what each one leaves out.
func TestRentableOfNothingNamesTheFiltersThatLeaveTheOffersOut(t *testing.T) {
	api := newFakeAPI(t)
	sized := `{"offers":[` +
		`{"id":1,"machine_id":11,"dph_total":0.44,"geolocation":"California, US","reliability2":0.99,"verification":"verified"},` +
		`{"id":2,"machine_id":12,"dph_total":0.80,"geolocation":"Sweden, SE","reliability2":0.99,"verification":"verified"},` +
		`{"id":3,"machine_id":13,"dph_total":0.90,"geolocation":"Estonia, EE","reliability2":0.90,"verification":"unverified"}]}`
	api.on(http.MethodPost, searchPath, inSequence(answer(200, `{"offers":[]}`), answer(200, sized)))
	p := newTestProvider(t, api, gpuSettings())

	supply, err := p.Rentable(context.Background(), gpuSpec(), 200)
	if err != nil {
		t.Fatal(err)
	}
	if supply.Count != 0 || supply.Why != "no vast offer of its size passes regions, max_price_per_hour, min_reliability, verified_only" {
		t.Fatalf("Rentable = %+v", supply)
	}
	for _, want := range []string{"3 offers of the size", "$0.440/h", "regions 2", "max_price_per_hour 2", "min_reliability 1", "verified_only 1"} {
		if !strings.Contains(supply.Detail, want) {
			t.Errorf("detail %q does not say %q", supply.Detail, want)
		}
	}
	searches := api.sent(http.MethodPost, searchPath)
	if len(searches) != 2 {
		t.Fatalf("%d searches, want the filtered one and one of the size alone", len(searches))
	}
	for _, key := range []string{"dph_total", "reliability2", "geolocation", "verified", "machine_id"} {
		if _, filtered := searches[1].Body[key]; filtered {
			t.Errorf("the search of the size alone filters on %s", key)
		}
	}
	for _, key := range []string{"gpu_name", "num_gpus", "cpu_cores_effective", "cpu_ram", "disk_space"} {
		if !reflect.DeepEqual(searches[1].Body[key], searches[0].Body[key]) {
			t.Errorf("the search of the size alone has %s = %v, the filtered one %v", key, searches[1].Body[key], searches[0].Body[key])
		}
	}
}

func TestRentableOfNothingSaysWhenTheVendorHasNoOfferOfTheSize(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, answer(200, `{"offers":[]}`))
	p := newTestProvider(t, api, gpuSettings())

	supply, err := p.Rentable(context.Background(), gpuSpec(), 200)
	if err != nil || supply.Count != 0 || supply.Why != "vast has no on-demand offer of its size (2 x RTX 4090, 4 cores, 16385 MB memory, 41 GB disk)" {
		t.Fatalf("Rentable = %+v, %v", supply, err)
	}
}

func TestRentableOfNothingSaysWhenThisAgentHoldsEveryOffer(t *testing.T) {
	api := newFakeAPI(t)
	ctx := context.Background()
	api.on(http.MethodPost, searchPath, offersOn(11))
	api.on(http.MethodPut, rentPath, answer(200, `{"success":true,"new_contract":9001}`))
	api.on(http.MethodGet, listPath, answer(200, instancesPage("", instanceJSON(9001, "hl-exp1-a2", ""))))
	p := newTestProvider(t, api, gpuSettings())
	if _, err := p.Create(ctx, gpuSpec()); err != nil {
		t.Fatal(err)
	}

	supply, err := p.Rentable(ctx, gpuSpec(), 200)
	if err != nil || supply.Count != 0 || !strings.Contains(supply.Why, "own instances hold every vast offer") {
		t.Fatalf("Rentable = %+v, %v", supply, err)
	}
}
