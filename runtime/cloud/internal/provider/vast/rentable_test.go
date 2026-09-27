package vast

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

func TestRentableCountsTheOffersCreateWouldChooseAmong(t *testing.T) {
	api := newFakeAPI(t)
	ctx := context.Background()
	api.on(http.MethodPost, searchPath, offersOn(11, 12, 13))
	api.on(http.MethodPut, rentPath, answer(200, `{"success":true,"new_contract":9001}`))
	api.on(http.MethodGet, listPath, answer(200, instancesPage("", instanceJSON(9001, "hl-exp1-a2", ""))))
	p := newTestProvider(t, api, gpuSettings())

	if count, err := p.Rentable(ctx, gpuSpec(), 200); err != nil || count != 3 {
		t.Fatalf("Rentable = %d, %v, want the 3 offers within the filters", count, err)
	}
	if count, err := p.Rentable(ctx, gpuSpec(), 2); err != nil || count != 2 {
		t.Fatalf("Rentable = %d, %v, want no more than asked about", count, err)
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
	if count, err := p.Rentable(ctx, gpuSpec(), 200); err != nil || count != 2 {
		t.Fatalf("Rentable with one offer held = %d, %v, want 2", count, err)
	}
	last := api.sent(http.MethodPost, searchPath)
	if limit := last[len(last)-1].Body["limit"]; limit != float64(201) {
		t.Errorf("Rentable asked for %v offers with one held, want 201", limit)
	}
}

func TestRentableIsNothingWhenNoOfferPassesTheFilters(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, answer(200, `{"offers":[]}`))
	p := newTestProvider(t, api, gpuSettings())

	if count, err := p.Rentable(context.Background(), gpuSpec(), 200); err != nil || count != 0 {
		t.Fatalf("Rentable = %d, %v", count, err)
	}
}
