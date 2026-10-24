package vast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

const networkName = "hl-net-ns-exp1-a1"

// clusterOffer is an offer of machine on cluster at price.
type clusterOffer struct {
	id, machine, cluster int
	price                float64
}

func clusterOffers(offers ...clusterOffer) http.HandlerFunc {
	var encoded []string
	for _, o := range offers {
		encoded = append(encoded, fmt.Sprintf(`{"id":%d,"machine_id":%d,"cluster_id":%d,"dph_total":%.2f}`, o.id, o.machine, o.cluster, o.price))
	}
	return answer(200, `{"offers":[`+strings.Join(encoded, ",")+`]}`)
}

// Cluster 1 is cheapest and has one machine, cluster 2 has two machines and cluster 3 has one.
func threeClusters() http.HandlerFunc {
	return clusterOffers(
		clusterOffer{7001, 11, 1, 0.10},
		clusterOffer{7002, 11, 1, 0.11},
		clusterOffer{7003, 21, 2, 0.20},
		clusterOffer{7004, 22, 2, 0.25},
		clusterOffer{7005, 31, 3, 0.05},
	)
}

func TestCreateNetworkMakesTheOverlayOnTheCheapestClusterWithAPlaceForEachInstance(t *testing.T) {
	for name, tc := range map[string]struct {
		spread  bool
		cluster float64
	}{
		"spread over machines": {true, 2},
		"on any offers":        {false, 1},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on(http.MethodPost, searchPath, threeClusters())
			overlays := api.serveOverlays()
			p := newTestProvider(t, api, gpuSettings())

			if err := p.CreateNetwork(context.Background(), networkName, []provider.Spec{gpuSpec(), gpuSpec()}, tc.spread); err != nil {
				t.Fatalf("CreateNetwork: %v", err)
			}
			search := api.sent(http.MethodPost, searchPath)[0].Body
			if got := search["cluster_id"]; !reflect.DeepEqual(got, map[string]any{"gt": float64(0)}) {
				t.Errorf("searched cluster_id %v, want offers on a cluster", got)
			}
			if got := search["gpu_name"]; !reflect.DeepEqual(got, map[string]any{"eq": "RTX 4090"}) {
				t.Errorf("searched gpu_name %v, want the spec's", got)
			}
			created := api.sent(http.MethodPost, overlayPath)
			if len(created) != 1 || !reflect.DeepEqual(created[0].Body, map[string]any{"cluster_id": tc.cluster, "name": overlayName(networkName)}) {
				t.Fatalf("created %+v, want %s on cluster %v", created, overlayName(networkName), tc.cluster)
			}
			if names := overlays.names(); !slices.Equal(names, []string{overlayName(networkName)}) {
				t.Errorf("overlays = %v", names)
			}
		})
	}
}

// Two specs of different sizes on one cluster each need a machine of their own: the matching
// gives the larger the one machine only it fits on.
func TestCreateNetworkGivesEachSpecAPlaceOfItsOwn(t *testing.T) {
	api := newFakeAPI(t)
	small, large := cpuSpec(), cpuSpec()
	large.CPUMillis = 64000
	api.on(http.MethodPost, searchPath, func(w http.ResponseWriter, r *http.Request) {
		var asked map[string]any
		_ = json.NewDecoder(r.Body).Decode(&asked)
		if cores, _ := asked["cpu_cores_effective"].(map[string]any); cores["gte"] == float64(64) {
			clusterOffers(clusterOffer{7002, 12, 4, 0.40})(w, r)
			return
		}
		clusterOffers(clusterOffer{7002, 12, 4, 0.40}, clusterOffer{7001, 11, 4, 0.10})(w, r)
	})
	api.serveOverlays()
	p := newTestProvider(t, api, gpuSettings())

	if err := p.CreateNetwork(context.Background(), networkName, []provider.Spec{small, large}, true); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if created := api.sent(http.MethodPost, overlayPath); len(created) != 1 || created[0].Body["cluster_id"] != float64(4) {
		t.Fatalf("created %+v, want the overlay on cluster 4", created)
	}
}

func TestCreateNetworkHasNoOfferWhenNoClusterHoldsEveryInstance(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, threeClusters())
	api.serveOverlays()
	p := newTestProvider(t, api, gpuSettings())

	err := p.CreateNetwork(context.Background(), networkName, []provider.Spec{gpuSpec(), gpuSpec(), gpuSpec()}, true)
	if !errors.Is(err, provider.ErrNoOffer) {
		t.Fatalf("CreateNetwork = %v, want ErrNoOffer", err)
	}
	if created := api.sent(http.MethodPost, overlayPath); len(created) != 0 {
		t.Errorf("created %+v, want no overlay", created)
	}
}

func TestCreateNetworkOfANameThatExistsMakesNothing(t *testing.T) {
	api := newFakeAPI(t)
	api.serveOverlays(overlay{ID: 1, Name: overlayName(networkName), ClusterID: 2})
	p := newTestProvider(t, api, gpuSettings())

	if err := p.CreateNetwork(context.Background(), networkName, []provider.Spec{gpuSpec(), gpuSpec()}, true); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if searches, created := api.sent(http.MethodPost, searchPath), api.sent(http.MethodPost, overlayPath); len(searches)+len(created) != 0 {
		t.Errorf("searches %d and creates %d, want none", len(searches), len(created))
	}
}

func TestNetworkedCountsTheMostOneClusterHolds(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, inSequence(threeClusters(), clusterOffers()))
	p := newTestProvider(t, api, gpuSettings())
	p.marketFor = marketInterval
	ctx := context.Background()

	for _, tc := range []struct {
		atMost int
		spread bool
		want   int
	}{{10, true, 2}, {10, false, 2}, {1, true, 1}} {
		if supply, err := p.Networked(ctx, gpuSpec(), tc.atMost, tc.spread); err != nil || supply != (provider.Supply{Count: tc.want}) {
			t.Errorf("Networked(%d, spread %v) = %+v, %v, want %d", tc.atMost, tc.spread, supply, err, tc.want)
		}
	}
	searches := api.sent(http.MethodPost, searchPath)
	if len(searches) != 1 || searches[0].Body["limit"] != float64(clusterSearchLimit) {
		t.Fatalf("searches = %+v, want one read of %d offers that stands", searches, clusterSearchLimit)
	}

	p.marketFor = 0
	supply, err := p.Networked(ctx, gpuSpec(), 10, true)
	if err != nil || supply.Count != 0 || supply.Why == "" {
		t.Fatalf("Networked with no offer on a cluster = %+v, %v, want 0 and why", supply, err)
	}
}

func TestNetworkedOffersNoneWithPrivateNetworksOff(t *testing.T) {
	api := newFakeAPI(t)
	settings := gpuSettings()
	settings.PrivateNetworks = false
	supply, err := newTestProvider(t, api, settings).Networked(context.Background(), gpuSpec(), 10, true)
	if err != nil || supply.Count != 0 || supply.Why == "" {
		t.Fatalf("Networked = %+v, %v, want 0 and why", supply, err)
	}
	if searches := api.sent(http.MethodPost, searchPath); len(searches) != 0 {
		t.Errorf("searches = %d, want none", len(searches))
	}
}

func TestCreateOnANetworkRentsOnItsClusterAndJoinsIt(t *testing.T) {
	api := newFakeAPI(t)
	api.serveOverlays(overlay{ID: 1, Name: overlayName(networkName), ClusterID: 2})
	api.on(http.MethodPost, searchPath, clusterOffers(clusterOffer{7001, 21, 2, 0.20}))
	api.on(http.MethodPut, rentPath, answer(200, `{"success":true,"new_contract":9001}`))
	joined := `{"id":9001,"label":"hl-exp1-a2","start_date":` + fixtureStart + `,"extra_env":[["HYPOTHESISLOOP_TOKEN","x"],["-n ` + overlayName(networkName) + `","1"]]}`
	api.on(http.MethodGet, listPath, answer(200, instancesPage("", joined)))
	api.on(http.MethodGet, showPath, answer(200, `{"instances":`+joined+`}`))
	p := newTestProvider(t, api, gpuSettings())
	spec := gpuSpec()
	spec.Network = networkName
	env := maps.Clone(spec.Env)

	id, err := p.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := api.sent(http.MethodPost, searchPath)[0].Body["cluster_id"]; !reflect.DeepEqual(got, map[string]any{"eq": float64(2)}) {
		t.Errorf("searched cluster_id %v, want the network's cluster 2", got)
	}
	sent := api.sent(http.MethodPut, rentPath)[0].Body["env"]
	want := map[string]any{"HYPOTHESISLOOP_TOKEN": env["HYPOTHESISLOOP_TOKEN"], "-n " + overlayName(networkName): "1"}
	if !reflect.DeepEqual(sent, want) {
		t.Errorf("rented with env %v, want %v", sent, want)
	}
	if !maps.Equal(spec.Env, env) {
		t.Errorf("Create changed the spec's env to %v", spec.Env)
	}
	got, exists, err := p.Get(context.Background(), id)
	if err != nil || !exists || got.PrivateAddress != "C.9001" {
		t.Fatalf("Get = %+v, %v, %v, want the address C.9001", got, exists, err)
	}
}

func TestCreateOnANetworkThatDoesNotExistRentsNothing(t *testing.T) {
	api := newFakeAPI(t)
	api.serveOverlays()
	p := newTestProvider(t, api, gpuSettings())
	spec := gpuSpec()
	spec.Network = networkName

	if _, err := p.Create(context.Background(), spec); err == nil || errors.Is(err, provider.ErrNoOffer) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Create = %v, want the network named missing", err)
	}
	if searches := api.sent(http.MethodPost, searchPath); len(searches) != 0 {
		t.Errorf("searches = %d, want none", len(searches))
	}
}

func TestAnInstanceOnNoNetworkHasNoAddress(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodGet, showPath, answer(200, `{"instances":{"id":9001,"label":"hl-exp1-a2","start_date":`+fixtureStart+`,"extra_env":[["-p 22:22","1"]]}}`))
	got, exists, err := newTestProvider(t, api, Settings{}).Get(context.Background(), "9001")
	if err != nil || !exists || got.PrivateAddress != "" {
		t.Fatalf("Get = %+v, %v, %v, want no address", got, exists, err)
	}
}

func TestDeleteNetworkIsRepeatable(t *testing.T) {
	api := newFakeAPI(t)
	overlays := api.serveOverlays(overlay{ID: 41, Name: overlayName(networkName), ClusterID: 2}, overlay{ID: 42, Name: overlayName("hl-net-ns-exp2-a1"), ClusterID: 2})
	p := newTestProvider(t, api, Settings{})

	for range 2 {
		if err := p.DeleteNetwork(context.Background(), networkName); err != nil {
			t.Fatalf("DeleteNetwork: %v", err)
		}
	}
	deletes := api.sent(http.MethodDelete, overlayPath)
	if len(deletes) != 1 || !reflect.DeepEqual(deletes[0].Body, map[string]any{"overlay_id": float64(41)}) {
		t.Fatalf("deletes = %+v, want one of overlay 41", deletes)
	}
	if names := overlays.names(); !slices.Equal(names, []string{overlayName("hl-net-ns-exp2-a1")}) {
		t.Errorf("overlays left = %v", names)
	}
}

func TestNetworksAreTheOverlaysUnderThePrefix(t *testing.T) {
	api := newFakeAPI(t)
	api.serveOverlays(
		overlay{ID: 1, Name: overlayName("hl-net-ns-exp2-a1")},
		overlay{ID: 2, Name: "someone-else"},
		overlay{ID: 3, Name: overlayName("hl-net-ns-exp1-a1")},
		overlay{ID: 4, Name: overlayName("hl-net-other-exp1-a1")},
		overlay{ID: 5, Name: "hl-net-ns-qz"},
	)
	names, err := newTestProvider(t, api, Settings{}).Networks(context.Background(), provider.NetworkPrefix("ns"))
	if err != nil || !slices.Equal(names, []string{"hl-net-ns-exp1-a1", "hl-net-ns-exp2-a1"}) {
		t.Fatalf("Networks = %v, %v", names, err)
	}
}

func TestOverlayNamesHaveNoDigitAndSpellTheirNetwork(t *testing.T) {
	for _, network := range []string{networkName, "hl-net-ns-0123456789-a12", "hl-net-quiet-q9-a1", "q", "qq0"} {
		spelt := overlayName(network)
		if !vendorOverlayName.MatchString(spelt) {
			t.Errorf("overlayName(%q) = %q, which the vendor refuses", network, spelt)
		}
		if back, ok := networkOf(spelt); !ok || back != network {
			t.Errorf("networkOf(%q) = %q, %v, want %q", spelt, back, ok, network)
		}
	}
	for _, spelt := range []string{"q", "hl-qz", "hl-q-"} {
		if network, ok := networkOf(spelt); ok {
			t.Errorf("networkOf(%q) = %q, want no network", spelt, network)
		}
	}
}

func TestCreateNetworkFailsWhenTheVendorRefusesIt(t *testing.T) {
	api := newFakeAPI(t)
	api.on(http.MethodPost, searchPath, threeClusters())
	api.on(http.MethodGet, overlayPath, answer(200, `[]`))
	api.on(http.MethodPost, overlayPath, answer(200, `{"success": false, "msg": "invalid network name"}`))
	p := newTestProvider(t, api, gpuSettings())

	err := p.CreateNetwork(context.Background(), networkName, []provider.Spec{gpuSpec()}, true)
	if err == nil || errors.Is(err, provider.ErrNoOffer) || !strings.Contains(err.Error(), "invalid network name") {
		t.Fatalf("CreateNetwork = %v, want the vendor's refusal", err)
	}
}
