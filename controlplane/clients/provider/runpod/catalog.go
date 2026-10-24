package runpod

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"math/bits"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

var (
	_ provider.Market    = (*Provider)(nil)
	_ provider.Networker = (*Provider)(nil)
)

// stock is the vendor's level of what it has of one kind in one data center. It publishes a
// level, not a count.
type stock struct {
	ID           string `json:"id"`
	Availability string `json:"availability"`
}

var levels = map[string]int{"NONE": 0, "LOW": 1, "MEDIUM": 2, "HIGH": 3}

type gpuType struct {
	ID    string `json:"id"`
	Price struct {
		Secure    float64 `json:"secure"`
		Community float64 `json:"community"`
	} `json:"price"`
	MaxCount struct {
		Secure    int `json:"secure"`
		Community int `json:"community"`
	} `json:"maxCount"`
	DataCenters []stock `json:"dataCenters"`
}

type cpuType struct {
	ID   string `json:"id"`
	VCPU struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"vcpu"`
	RAMGBPerVCPU float64 `json:"ramGbPerVcpu"`
	Price        struct {
		SecurePerVCPU float64 `json:"securePerVcpu"`
	} `json:"price"`
	DataCenters []stock `json:"dataCenters"`
}

// offer is one way to rent a spec: a GPU type or a CPU flavor at a size, and the data centers
// the vendor has stock of it in, the most stocked first.
type offer struct {
	gpu          string
	gpus         int
	cpu          string
	vcpus        int
	pricePerHour float64
	dataCenters  []stock
}

const gib = 1 << 30

// offers are the ways to rent spec within the settings, the cheapest first, each in the data
// centers it may be rented in that have stock of it. why says what leaves out the vendor's
// machines when there are none.
func (p *Provider) offers(ctx context.Context, spec provider.Spec) (offers []offer, why string, err error) {
	if spec.AcceleratorCount > 0 {
		return p.gpuOffers(ctx, spec)
	}
	return p.cpuOffers(ctx, spec)
}

func (p *Provider) gpuOffers(ctx context.Context, spec provider.Spec) ([]offer, string, error) {
	accelerator, err := p.cfg.accelerator(spec.AcceleratorType)
	if err != nil {
		return nil, "", err
	}
	query := url.Values{"include": {"AVAILABILITY"}, "product": {"POD"}, "count": {strconv.Itoa(spec.AcceleratorCount)}, "cloud": {p.cfg.Cloud}}
	var found gpuType
	if err := p.call(ctx, http.MethodGet, "/v2/catalog/gpus/"+url.PathEscape(accelerator.GPUID)+"?"+query.Encode(), nil, &found); err != nil {
		return nil, "", err
	}
	price, most := found.Price.Secure, found.MaxCount.Secure
	if p.cfg.Cloud == "COMMUNITY" {
		price, most = found.Price.Community, found.MaxCount.Community
	}
	o := offer{gpu: found.ID, gpus: spec.AcceleratorCount, pricePerHour: price * float64(spec.AcceleratorCount)}
	switch {
	case spec.AcceleratorCount > most:
		return nil, fmt.Sprintf("Runpod has no machine with that many %s in its %s cloud", found.ID, p.cfg.Cloud), nil
	case o.pricePerHour > p.cfg.MaxPricePerHour:
		return nil, fmt.Sprintf("%d of %s cost $%.4f/h, more than this Runpod account's max_price_per_hour $%.4f", o.gpus, found.ID, o.pricePerHour, p.cfg.MaxPricePerHour), nil
	}
	o.dataCenters = p.stocked(found.DataCenters)
	if len(o.dataCenters) == 0 {
		return nil, fmt.Sprintf("Runpod has no stock of %s in the %s cloud in the data centers this account rents in", found.ID, p.cfg.Cloud), nil
	}
	return []offer{o}, "", nil
}

func (p *Provider) cpuOffers(ctx context.Context, spec provider.Spec) ([]offer, string, error) {
	var listed struct {
		CPUs []cpuType `json:"cpus"`
	}
	if err := p.call(ctx, http.MethodGet, "/v2/catalog/cpus?include=AVAILABILITY&product=POD", nil, &listed); err != nil {
		return nil, "", err
	}
	var offers []offer
	sized, priced := 0, 0
	for _, flavor := range listed.CPUs {
		if len(p.cfg.CPUFlavors) > 0 && !slices.Contains(p.cfg.CPUFlavors, flavor.ID) {
			continue
		}
		vcpus, fits := flavor.size(spec)
		if !fits {
			continue
		}
		sized++
		o := offer{cpu: flavor.ID, vcpus: vcpus, pricePerHour: flavor.Price.SecurePerVCPU * float64(vcpus)}
		if o.pricePerHour > p.cfg.MaxPricePerHour {
			continue
		}
		priced++
		if o.dataCenters = p.stocked(flavor.DataCenters); len(o.dataCenters) > 0 {
			offers = append(offers, o)
		}
	}
	slices.SortStableFunc(offers, func(a, b offer) int { return cmp.Compare(a.pricePerHour, b.pricePerHour) })
	switch {
	case len(offers) > 0:
		return offers, "", nil
	case sized == 0:
		return nil, "no Runpod CPU flavor this account rents holds the shape's cores and memory", nil
	case priced == 0:
		return nil, "every Runpod CPU flavor that holds the shape costs more than this account's max_price_per_hour", nil
	}
	return nil, "Runpod has no stock of a CPU flavor that holds the shape in the data centers this account rents in", nil
}

// size is the vCPU count of the smallest pod of the flavor that holds spec's cores and memory: a
// power of two, as the vendor rents them, with memory in proportion.
func (c cpuType) size(spec provider.Spec) (int, bool) {
	if c.RAMGBPerVCPU <= 0 {
		return 0, false
	}
	need := max(2, int(provider.CeilDiv(spec.CPUMillis, 1000)), int(math.Ceil(float64(provider.CeilDiv(spec.MemoryBytes, gib))/c.RAMGBPerVCPU)))
	vcpus := 1 << bits.Len(uint(need-1))
	vcpus = max(vcpus, c.VCPU.Min)
	return vcpus, vcpus <= c.VCPU.Max
}

// stocked are the data centers of found that the settings allow and the vendor has stock in, the
// most stocked first.
func (p *Provider) stocked(found []stock) []stock {
	var kept []stock
	for _, s := range found {
		if levels[s.Availability] > 0 && p.cfg.allows(s.ID) {
			kept = append(kept, s)
		}
	}
	slices.SortStableFunc(kept, func(a, b stock) int { return cmp.Compare(levels[b.Availability], levels[a.Availability]) })
	return kept
}

// Rentable counts atMost when the vendor has stock of a way to rent spec in a data center the
// settings allow, and nothing otherwise: it publishes a level of stock and no count. A create can
// still find none, which is ErrNoOffer.
func (p *Provider) Rentable(ctx context.Context, spec provider.Spec, atMost int) (provider.Supply, error) {
	offers, why, err := p.offers(ctx, spec)
	if err != nil {
		return provider.Supply{}, err
	}
	if len(offers) == 0 {
		return provider.Supply{Why: why}, nil
	}
	return provider.Supply{Count: atMost}, nil
}

// interfaceName is the interface the global network is on inside a pod.
const interfaceName = "podnet1"

func (p *Provider) Interface() string { return interfaceName }

// Within is a zone: a place is one data center, whose pods meet at 100 Mbps on the global network.
func (p *Provider) Within() string { return "zone" }

// networkable says why spec cannot be rented on the global network, empty when it can.
func networkable(spec provider.Spec) string {
	if spec.AcceleratorCount == 0 {
		return "Runpod's global networking takes GPU pods only"
	}
	return ""
}

// networked are the data centers on the global network where the vendor has stock of a way to
// rent spec, the most stocked first, and why there are none.
func (p *Provider) networked(ctx context.Context, spec provider.Spec, global []string) ([]stock, string, error) {
	offers, why, err := p.offers(ctx, spec)
	if err != nil || len(offers) == 0 {
		return nil, why, err
	}
	var places []stock
	for _, dc := range offers[0].dataCenters {
		if slices.Contains(global, dc.ID) {
			places = append(places, dc)
		}
	}
	if len(places) == 0 {
		return nil, fmt.Sprintf("Runpod has no stock of %s in a data center on its global network that this account rents in", offers[0].gpu), nil
	}
	return places, "", nil
}

// globalNetwork are the ids of the data centers on the vendor's global network.
func (p *Provider) globalNetwork(ctx context.Context) ([]string, error) {
	var listed struct {
		DataCenters []struct {
			ID string `json:"id"`
		} `json:"dataCenters"`
	}
	if err := p.call(ctx, http.MethodGet, "/v2/catalog/datacenters?globalNetwork=true", nil, &listed); err != nil {
		return nil, err
	}
	ids := make([]string, len(listed.DataCenters))
	for n, dc := range listed.DataCenters {
		ids[n] = dc.ID
	}
	return ids, nil
}

// Networked counts atMost when one data center on the global network has stock of spec, as
// Rentable does.
func (p *Provider) Networked(ctx context.Context, spec provider.Spec, atMost int) (provider.Supply, error) {
	if why := networkable(spec); why != "" {
		return provider.Supply{Why: why}, nil
	}
	global, err := p.globalNetwork(ctx)
	if err != nil {
		return provider.Supply{}, err
	}
	places, why, err := p.networked(ctx, spec, global)
	if err != nil {
		return provider.Supply{}, err
	}
	if len(places) == 0 {
		return provider.Supply{Why: why}, nil
	}
	return provider.Supply{Count: atMost}, nil
}

// sized is what of a spec its offers are found by.
type sized struct {
	acceleratorType        string
	accelerators           int
	cpuMillis, memoryBytes int64
}

func sizeOf(spec provider.Spec) sized {
	return sized{spec.AcceleratorType, spec.AcceleratorCount, spec.CPUMillis, spec.MemoryBytes}
}

// Place is the data center on the global network with stock of every one of specs, the one whose
// least stocked spec is the most stocked. Specs of one size are looked up once.
func (p *Provider) Place(ctx context.Context, specs []provider.Spec) (string, error) {
	if len(specs) == 0 {
		return "", fmt.Errorf("runpod: a place for no instance")
	}
	for _, spec := range specs {
		if why := networkable(spec); why != "" {
			return "", fmt.Errorf("runpod: %w for %q: %s", provider.ErrNoOffer, spec.Name, why)
		}
	}
	global, err := p.globalNetwork(ctx)
	if err != nil {
		return "", err
	}
	// level is the stock of the least stocked spec in each data center that has all of them.
	level := map[string]int{}
	looked := map[sized]bool{}
	for _, spec := range specs {
		size := sizeOf(spec)
		if looked[size] {
			continue
		}
		first := len(looked) == 0
		looked[size] = true
		places, why, err := p.networked(ctx, spec, global)
		if err != nil {
			return "", err
		}
		if len(places) == 0 {
			return "", fmt.Errorf("runpod: %w for %q: %s", provider.ErrNoOffer, spec.Name, why)
		}
		next := map[string]int{}
		for _, dc := range places {
			if prior, kept := level[dc.ID]; first || kept {
				next[dc.ID] = levels[dc.Availability]
				if !first {
					next[dc.ID] = min(prior, next[dc.ID])
				}
			}
		}
		level = next
	}
	if len(level) == 0 {
		return "", fmt.Errorf("runpod: %w: no data center on the global network has stock of each of the %d pods", provider.ErrNoOffer, len(specs))
	}
	best := ""
	for dc, l := range level {
		if best == "" || l > level[best] || (l == level[best] && dc < best) {
			best = dc
		}
	}
	return best, nil
}
