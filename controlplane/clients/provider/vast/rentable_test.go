package vast

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
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

	// The search goes on listing the offer this process now holds: it is not there to rent,
	// in the read that stands as in a new one, which asks for one more.
	if supply, err := p.Rentable(ctx, gpuSpec(), 200); err != nil || supply.Count != 2 {
		t.Fatalf("Rentable with one offer held = %+v, %v, want 2", supply, err)
	}
	p.marketFor = 0
	if supply, err := p.Rentable(ctx, gpuSpec(), 200); err != nil || supply.Count != 2 {
		t.Fatalf("Rentable with one offer held, read again = %+v, %v, want 2", supply, err)
	}
	last := api.sent(http.MethodPost, searchPath)
	if limit := last[len(last)-1].Body["limit"]; limit != float64(201) {
		t.Errorf("Rentable asked for %v offers with one held, want 201", limit)
	}
}

// No offer passes the filters, and there are offers of the size: the answer names the filters
// that leave them out, with no figures, and the log counts what each one leaves out.
func TestRentableOfNothingNamesTheFiltersThatLeaveTheOffersOut(t *testing.T) {
	api := newFakeAPI(t)
	sized := `{"offers":[` +
		`{"id":1,"machine_id":11,"dph_total":0.44,"geolocation":"California, US","reliability2":0.99,"verification":"verified"},` +
		`{"id":2,"machine_id":12,"dph_total":0.80,"geolocation":"Sweden, SE","reliability2":0.99,"verification":"verified"},` +
		`{"id":3,"machine_id":13,"dph_total":0.90,"geolocation":"Estonia, EE","reliability2":0.90,"verification":"unverified"}]}`
	api.on(http.MethodPost, searchPath, inSequence(answer(200, `{"offers":[]}`), answer(200, sized)))
	p := newTestProvider(t, api, gpuSettings())
	var logged strings.Builder
	p.log = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }

	supply, err := p.Rentable(context.Background(), gpuSpec(), 200)
	if err != nil {
		t.Fatal(err)
	}
	if supply.Count != 0 || supply.Why != "no vast offer of its size passes regions, max_price_per_hour, min_reliability, verified_only" {
		t.Fatalf("Rentable = %+v", supply)
	}
	for _, want := range []string{"3 offers of the size", "$0.440/h", "regions 2", "max_price_per_hour 2", "min_reliability 1", "verified_only 1"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log %q does not say %q", logged.String(), want)
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
	if err != nil || supply.Count != 0 || supply.Why != "vast has no on-demand offer of its size (2 x RTX 4090, amd64, 4 cores, 16385 MB memory, 41 GB disk)" {
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

// A read stands for marketInterval: asked again within it, Rentable answers from it, unless more
// are wanted than it asked for and it found as many. One that failed does not stand.
func TestRentableAnswersFromAReadWithinItsInterval(t *testing.T) {
	api := newFakeAPI(t)
	ctx := context.Background()
	api.on(http.MethodPost, searchPath, inSequence(answer(500, `{}`), offersOn(11, 12), offersOn(11, 12), offersOn(11, 12, 13)))
	p := newTestProvider(t, api, gpuSettings())
	p.marketFor = time.Hour
	searches := func() int { return len(api.sent(http.MethodPost, searchPath)) }

	if _, err := p.Rentable(ctx, gpuSpec(), 2); err == nil {
		t.Fatal("a failed search answered")
	}
	for range 3 {
		if supply, err := p.Rentable(ctx, gpuSpec(), 2); err != nil || supply.Count != 2 {
			t.Fatalf("Rentable = %+v, %v", supply, err)
		}
	}
	if searches() != 2 {
		t.Fatalf("%d searches, want the failed one and one that stands", searches())
	}
	if supply, err := p.Rentable(ctx, gpuSpec(), 5); err != nil || supply.Count != 2 || searches() != 3 {
		t.Fatalf("asked for more than the read found as many of: %+v, %v, %d searches", supply, err, searches())
	}
	if supply, err := p.Rentable(ctx, gpuSpec(), 9); err != nil || supply.Count != 2 || searches() != 3 {
		t.Fatalf("the read found fewer than it asked for, which answers any ask: %+v, %v, %d searches", supply, err, searches())
	}
	p.marketFor = 0
	if supply, err := p.Rentable(ctx, gpuSpec(), 9); err != nil || supply.Count != 3 || searches() != 4 {
		t.Fatalf("an old read: %+v, %v, %d searches", supply, err, searches())
	}
}

// A read made for Rentable answers Create when it asked for as many offers, and Rentable a read
// Create made: the cache is keyed by the search, whoever asks.
func TestRentableAndCreateShareTheReadOfTheirSearch(t *testing.T) {
	api := newFakeAPI(t)
	ctx := context.Background()
	api.on(http.MethodPost, searchPath, offersOn(11, 12, 13))
	api.on(http.MethodPut, rentPath, answer(200, `{"success":true,"new_contract":9001}`))
	api.on(http.MethodGet, listPath, answer(200, instancesPage("", instanceJSON(9001, "hl-exp1-a2", ""))))
	p := newTestProvider(t, api, gpuSettings())
	p.marketFor = time.Hour

	if supply, err := p.Rentable(ctx, gpuSpec(), offerSearchLimit); err != nil || supply.Count != 3 {
		t.Fatalf("Rentable = %+v, %v", supply, err)
	}
	if _, err := p.Create(ctx, gpuSpec()); err != nil {
		t.Fatal(err)
	}
	if supply, err := p.Rentable(ctx, gpuSpec(), 2); err != nil || supply.Count != 2 {
		t.Fatalf("Rentable after a rent = %+v, %v, want the two offers not held", supply, err)
	}
	if n := len(api.sent(http.MethodPost, searchPath)); n != 1 {
		t.Fatalf("%d searches, want the one read every answer came from", n)
	}
}

// Rentable asks for as many offers as it is asked about, on top of the ones held: a small
// demand is a small read.
func TestRentableAsksForNoMoreRowsThanItIsAskedAbout(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, offersOn(11, 12, 13))
	p := newTestProvider(t, api, gpuSettings())
	if _, err := p.Rentable(context.Background(), gpuSpec(), 3); err != nil {
		t.Fatal(err)
	}
	if limit := api.sent(http.MethodPost, searchPath)[0].Body["limit"]; limit != float64(3) {
		t.Fatalf("asked for %v rows, want 3", limit)
	}
}

// The reason a size has nothing to rent is read again only after shortfallInterval, however
// often the empty search is.
func TestTheSearchOfASizeAloneStandsLongerThanTheSearch(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, answer(200, `{"offers":[]}`))
	p := newTestProvider(t, api, gpuSettings())
	for range 4 {
		if supply, err := p.Rentable(context.Background(), gpuSpec(), 2); err != nil || supply.Why == "" {
			t.Fatalf("Rentable = %+v, %v", supply, err)
		}
	}
	searches := api.sent(http.MethodPost, searchPath)
	if len(searches) != 5 {
		t.Fatalf("%d searches, want four filtered ones and one of the size alone", len(searches))
	}
	for _, s := range searches[2:] {
		if _, filtered := s.Body["dph_total"]; !filtered {
			t.Fatal("the size alone was searched again within its interval")
		}
	}
}

// The day's budget bounds the rows read: a search that could pass it is not made, the answer says
// why and Create finds no offer, and the log says so once. A search spends the rows it was
// answered with, a failed one none.
func TestOfferSearchesStopAtTheDaysBudget(t *testing.T) {
	api := newFakeAPI(t)
	ctx := context.Background()
	api.on(http.MethodPost, searchPath, inSequence(answer(500, `{}`), offersOn(11, 12)))
	p := newTestProvider(t, api, gpuSettings())
	p.searches.budget = 26
	var logged strings.Builder
	p.log = func(format string, args ...any) { fmt.Fprintf(&logged, format+"\n", args...) }

	if _, err := p.Rentable(ctx, gpuSpec(), 20); err == nil {
		t.Fatal("a failed search answered")
	}
	if supply, err := p.Rentable(ctx, gpuSpec(), 20); err != nil || supply.Count != 2 {
		t.Fatalf("within the budget once the failed search gave its rows back: %+v, %v", supply, err)
	}
	searched := len(api.sent(http.MethodPost, searchPath))
	_, err := p.Rentable(ctx, gpuSpec(), 29)
	if !errors.Is(err, errSearchBudget) || !strings.Contains(err.Error(), "00:00 UTC") {
		t.Fatalf("past the budget: %v", err)
	}
	if _, err := p.Create(ctx, gpuSpec()); !errors.Is(err, provider.ErrNoOffer) || !errors.Is(err, errSearchBudget) {
		t.Fatalf("Create past the budget: %v, want no offer for the budget", err)
	}
	if n := len(api.sent(http.MethodPost, searchPath)); n != searched {
		t.Fatalf("%d searches past the budget", n-searched)
	}
	if n := strings.Count(logged.String(), "offer searches stop"); n != 1 {
		t.Fatalf("logged the budget %d times: %q", n, logged.String())
	}
}

// A Create that found every offer of its read taken drops the read, so the next one searches.
func TestACreateThatFindsEveryOfferTakenSearchesAgainNextTime(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, offersOn(11))
	api.on(http.MethodPut, rentPath, answer(400, taken))
	p := newTestProvider(t, api, gpuSettings())
	p.marketFor = time.Hour
	for range 2 {
		if _, err := p.Create(context.Background(), gpuSpec()); !errors.Is(err, provider.ErrNoOffer) {
			t.Fatalf("err = %v", err)
		}
	}
	if n := len(api.sent(http.MethodPost, searchPath)); n != 2 {
		t.Fatalf("%d searches, want one per Create", n)
	}
}
