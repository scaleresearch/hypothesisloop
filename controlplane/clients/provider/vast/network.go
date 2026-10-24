package vast

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

var _ provider.Networker = (*Provider)(nil)

const (
	overlayPath = "/api/v0/overlay/"
	// overlayFlag starts the environment key an instance joins an overlay at create with,
	// "-n <name>": "1", as the vendor's CLI sends it.
	overlayFlag = "-n "
	// clusterSearchLimit is how many of the cheapest offers on a physical cluster one read of a
	// size asks for.
	clusterSearchLimit = 100
	// overlayInterface is the interface an overlay is on inside an instance; the internet is on
	// eth1.
	overlayInterface = "eth0"
	// overlayEscape starts the spelling of a digit in an overlay name.
	overlayEscape = 'q'
)

// overlayName is network's name as the vendor takes it, which refuses any digit: each digit d is
// spelt overlayEscape and the d-th letter, and overlayEscape itself twice. Every network name has
// one overlay name, and a prefix of a network name spells a prefix of its overlay name.
func overlayName(network string) string {
	var spelt strings.Builder
	for _, c := range network {
		switch {
		case c >= '0' && c <= '9':
			spelt.WriteRune(overlayEscape)
			spelt.WriteRune('a' + c - '0')
		case c == overlayEscape:
			spelt.WriteRune(overlayEscape)
			spelt.WriteRune(overlayEscape)
		default:
			spelt.WriteRune(c)
		}
	}
	return spelt.String()
}

// networkOf is the network name overlayName spelt as name, and false for a name it does not spell.
func networkOf(name string) (string, bool) {
	var network strings.Builder
	for n := 0; n < len(name); n++ {
		if name[n] != overlayEscape {
			network.WriteByte(name[n])
			continue
		}
		n++
		switch {
		case n == len(name):
			return "", false
		case name[n] == overlayEscape:
			network.WriteByte(overlayEscape)
		case name[n] >= 'a' && name[n] <= 'j':
			network.WriteByte('0' + name[n] - 'a')
		default:
			return "", false
		}
	}
	return network.String(), true
}

// overlay is a private network on one physical cluster of the vendor's. Name is the overlay name.
type overlay struct {
	ID        int64  `json:"overlay_id"`
	Name      string `json:"name"`
	ClusterID int64  `json:"cluster_id"`
}

// answered is how the overlay calls answer, with 200 also when they refuse.
type answered struct {
	Success bool   `json:"success"`
	Message string `json:"msg"`
}

func (p *Provider) Interface() string { return overlayInterface }

func (p *Provider) overlays(ctx context.Context) ([]overlay, error) {
	var found []overlay
	if _, err := p.call(ctx, http.MethodGet, overlayPath, nil, &found); err != nil {
		return nil, err
	}
	return found, nil
}

func (p *Provider) overlay(ctx context.Context, name string) (overlay, bool, error) {
	found, err := p.overlays(ctx)
	if err != nil {
		return overlay{}, false, err
	}
	for _, o := range found {
		if o.Name == overlayName(name) {
			return o, true, nil
		}
	}
	return overlay{}, false, nil
}

func (p *Provider) Networks(ctx context.Context, prefix string) ([]string, error) {
	found, err := p.overlays(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range found {
		if name, ours := networkOf(o.Name); ours && strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// DeleteNetwork deletes the overlay by its id. The vendor removes the instances still on an
// overlay it deletes, which the caller has destroyed before.
func (p *Provider) DeleteNetwork(ctx context.Context, name string) error {
	found, exists, err := p.overlay(ctx, name)
	if err != nil || !exists {
		return err
	}
	var deleted answered
	if _, err := p.call(ctx, http.MethodDelete, overlayPath, map[string]int64{"overlay_id": found.ID}, &deleted); err != nil {
		return fmt.Errorf("vast: delete network %q: %w", name, err)
	}
	if !deleted.Success {
		return fmt.Errorf("vast: delete network %q was refused: %s", name, deleted.Message)
	}
	p.log("deleted network %s on cluster %d: %s", name, found.ClusterID, deleted.Message)
	return nil
}

// CreateNetwork makes the overlay on the cheapest physical cluster that has a place for every one
// of specs.
func (p *Provider) CreateNetwork(ctx context.Context, name string, specs []provider.Spec, spread bool) error {
	if err := provider.ValidateNetworkName(name); err != nil {
		return fmt.Errorf("vast: %w", err)
	}
	if _, exists, err := p.overlay(ctx, name); err != nil || exists {
		return err
	}
	cluster, err := p.site(ctx, name, specs, spread)
	if err != nil {
		return err
	}
	var created answered
	if _, err := p.call(ctx, http.MethodPost, overlayPath, map[string]any{"cluster_id": cluster, "name": overlayName(name)}, &created); err != nil {
		return fmt.Errorf("vast: create network %q on cluster %d: %w", name, cluster, err)
	}
	if !created.Success {
		return fmt.Errorf("vast: create network %q on cluster %d was refused: %s", name, cluster, created.Message)
	}
	p.log("created network %s on cluster %d: %s", name, cluster, created.Message)
	return nil
}

// Networked is the most instances of spec one physical cluster has offers for, on machines of
// their own when spread.
func (p *Provider) Networked(ctx context.Context, spec provider.Spec, atMost int, spread bool) (provider.Supply, error) {
	if !p.cfg.PrivateNetworks {
		return provider.Supply{Why: "private networks are off in this vast account's settings"}, nil
	}
	found, err := p.clustered(ctx, spec)
	if err != nil {
		return provider.Supply{}, err
	}
	if len(found) == 0 {
		return provider.Supply{Why: "no vast offer within the filters is on a physical cluster"}, nil
	}
	open := p.held.free(found)
	if len(open) == 0 {
		return provider.Supply{Why: "this agent's own instances hold every vast offer on a physical cluster within the filters"}, nil
	}
	places := map[int64]map[int64]bool{}
	for _, o := range open {
		if places[o.ClusterID] == nil {
			places[o.ClusterID] = map[int64]bool{}
		}
		places[o.ClusterID][place(o, spread)] = true
	}
	most := 0
	for _, taken := range places {
		most = max(most, len(taken))
	}
	return provider.Supply{Count: min(atMost, most)}, nil
}

// clustered is offerQuery's search among the offers on a physical cluster.
func (p *Provider) clustered(ctx context.Context, spec provider.Spec) ([]offer, error) {
	query, err := p.offerQuery(spec)
	if err != nil {
		return nil, err
	}
	query["cluster_id"] = map[string]any{"gt": 0}
	found, _, err := p.offers(ctx, query, clusterSearchLimit, p.marketFor)
	return found, err
}

// place is what one instance takes of an offer: the offer, or its whole machine when spread.
func place(o offer, spread bool) int64 {
	if spread {
		return o.MachineID
	}
	return o.ID
}

// site is the cheapest physical cluster with a place of its own for every one of specs, priced by
// the cheapest offer of each spec there.
func (p *Provider) site(ctx context.Context, name string, specs []provider.Spec, spread bool) (int64, error) {
	open := make([][]offer, len(specs))
	for s, spec := range specs {
		found, err := p.clustered(ctx, spec)
		if errors.Is(err, errSearchBudget) {
			return 0, fmt.Errorf("vast: %w for network %q: %w", provider.ErrNoOffer, name, err)
		}
		if err != nil {
			return 0, err
		}
		open[s] = p.held.free(found)
	}
	var clusters []int64
	for _, offers := range open {
		for _, o := range offers {
			clusters = append(clusters, o.ClusterID)
		}
	}
	slices.Sort(clusters)
	best, cheapest := int64(0), math.Inf(1)
	for _, cluster := range slices.Compact(clusters) {
		if cost, fits := fit(open, cluster, spread); fits && cost < cheapest {
			best, cheapest = cluster, cost
		}
	}
	if best == 0 {
		return 0, fmt.Errorf("vast: %w for network %q: no physical cluster has a place for each of its %d instances within the filters", provider.ErrNoOffer, name, len(specs))
	}
	return best, nil
}

// fit prices cluster for the specs whose offers open holds, and says whether each of them has a
// place of its own there.
func fit(open [][]offer, cluster int64, spread bool) (float64, bool) {
	options := make([][]int64, len(open))
	cost := 0.0
	for s, offers := range open {
		cheapest := math.Inf(1)
		for _, o := range offers {
			if o.ClusterID == cluster {
				options[s] = append(options[s], place(o, spread))
				cheapest = min(cheapest, o.PricePerHour)
			}
		}
		if len(options[s]) == 0 {
			return 0, false
		}
		cost += cheapest
	}
	return cost, matched(options)
}

// matched says whether each of options can be given one of its places that no other is given.
func matched(options [][]int64) bool {
	holder := map[int64]int{}
	var assign func(s int, tried map[int64]bool) bool
	assign = func(s int, tried map[int64]bool) bool {
		for _, at := range options[s] {
			if tried[at] {
				continue
			}
			tried[at] = true
			if other, held := holder[at]; !held || assign(other, tried) {
				holder[at] = s
				return true
			}
		}
		return false
	}
	for s := range options {
		if !assign(s, map[int64]bool{}) {
			return false
		}
	}
	return true
}

// createQuery is offerQuery, within the physical cluster of the spec's network when it has one.
func (p *Provider) createQuery(ctx context.Context, spec provider.Spec) (map[string]any, error) {
	query, err := p.offerQuery(spec)
	if err != nil || spec.Network == "" {
		return query, err
	}
	joined, exists, err := p.overlay(ctx, spec.Network)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("vast: spec %q joins network %q, which does not exist", spec.Name, spec.Network)
	}
	query["cluster_id"] = map[string]any{"eq": joined.ClusterID}
	return query, nil
}
