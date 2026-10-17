package vast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

const (
	bytesPerMB = 1 << 20
	bytesPerGB = 1 << 30

	// offerSearchLimit is how many of the cheapest offers are asked for, enough to find
	// offerCandidates different machines among them.
	offerSearchLimit = 25
	// offerCandidates is how many machines one Create chooses among.
	offerCandidates = 5
	// sizeSearchLimit is how many offers of a size, cheapest first, Rentable reads to say which
	// filters leave them out when none passes all of them.
	sizeSearchLimit = 25
)

type offer struct {
	ID           int64   `json:"id"`
	MachineID    int64   `json:"machine_id"`
	PricePerHour float64 `json:"dph_total"`
	// "California, US": the country code comes last.
	Geolocation  string  `json:"geolocation"`
	Reliability  float64 `json:"reliability2"`
	Verification string  `json:"verification"`
	Architecture string  `json:"cpu_arch"`
}

// architectures are the values of an offer's cpu_arch. In October 2026 about 28 offers were
// arm64 (GB10, GH200, GB300) beside thousands of amd64.
var architectures = []string{"amd64", "arm64"}

func (p *Provider) Architectures() []string { return slices.Clone(architectures) }

// sizeQuery builds the search for the cheapest on-demand offers of the spec's size, accelerator
// and architecture, before any of the settings' filters. The vendor rents no machine without a GPU (a
// search for zero GPUs answers with nothing, or with storage offers that cannot be rented as
// instances), so a CPU-only spec takes the cheapest offer of any GPU model.
func (p *Provider) sizeQuery(spec provider.Spec) (map[string]any, error) {
	if !slices.Contains(architectures, spec.Architecture) {
		return nil, fmt.Errorf("vast: spec %q runs on %q, and vast's offers are of %s", spec.Name, spec.Architecture, strings.Join(architectures, ", "))
	}
	query := map[string]any{
		"cpu_arch":            map[string]any{"eq": spec.Architecture},
		"type":                "ondemand",
		"rentable":            map[string]any{"eq": true},
		"rented":              map[string]any{"eq": false},
		"cpu_cores_effective": map[string]any{"gte": provider.CeilDiv(spec.CPUMillis, 1000)},
		"cpu_ram":             map[string]any{"gte": provider.CeilDiv(spec.MemoryBytes, bytesPerMB)},
		"disk_space":          map[string]any{"gte": diskGB(spec)},
		// The quoted price covers the disk actually allocated.
		"allocated_storage": diskGB(spec),
		"order":             [][]string{{"dph_total", "asc"}},
		"limit":             offerSearchLimit,
	}
	if spec.AcceleratorCount == 0 {
		query["num_gpus"] = map[string]any{"gte": 1}
		return query, nil
	}
	accelerator, err := p.cfg.accelerator(spec.AcceleratorType)
	if err != nil {
		return nil, err
	}
	query["num_gpus"] = map[string]any{"eq": spec.AcceleratorCount}
	query["gpu_name"] = map[string]any{"eq": accelerator.GPUName}
	if accelerator.MinGPURAMMB > 0 {
		query["gpu_ram"] = map[string]any{"gte": accelerator.MinGPURAMMB}
	}
	return query, nil
}

// offerQuery builds the search for the cheapest on-demand offers that fit the spec and pass every
// filter of the settings.
func (p *Provider) offerQuery(spec provider.Spec) (map[string]any, error) {
	query, err := p.sizeQuery(spec)
	if err != nil {
		return nil, err
	}
	query["dph_total"] = map[string]any{"lte": p.cfg.MaxPricePerHour}
	query["reliability2"] = map[string]any{"gte": p.cfg.MinReliability}
	excluded := append([]int64(nil), p.cfg.ExcludedMachines...)
	for _, host := range spec.AvoidHosts {
		machine, err := strconv.ParseInt(host, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("vast: spec %q avoids host %q, which is no machine id of this vendor's", spec.Name, host)
		}
		excluded = append(excluded, machine)
	}
	if len(excluded) > 0 {
		query["machine_id"] = map[string]any{"notin": excluded}
	}
	if len(p.cfg.Regions) > 0 {
		query["geolocation"] = map[string]any{"in": p.cfg.Regions}
	}
	if p.cfg.VerifiedOnly {
		query["verified"] = map[string]any{"eq": true}
	}
	return query, nil
}

// held is the one place an offer is handed out from: the offers this process is renting right
// now and the ones its own instances hold. The vendor's search goes on listing an offer whose GPU
// is rented, ours included, for as long as the instance lives, so without it every job of a pass
// is sent to the same few offers and all but one are refused.
//
// A process that starts without it asks for an offer one of its instances holds, is told the
// offer is taken and passes over it. Until those instances are gone Rentable counts their offers
// as rentable, since nothing the adapter reads says which offer an instance was rented from.
type held struct {
	mu     sync.Mutex
	offers map[int64]holder
}

// holder is who has an offer: the instance rented from it, or none yet while the rent is under
// way. listed says a read of the account has shown that instance.
type holder struct {
	instance string
	listed   bool
}

// claim takes id for one rental. It is false when the offer is already claimed or held.
func (h *held) claim(id int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, taken := h.offers[id]; taken {
		return false
	}
	h.offers[id] = holder{}
	return true
}

// settle ends the claim on id: the offer is held by instance, or free again when the rent
// failed and instance is empty.
func (h *held) settle(id int64, instance string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if instance == "" {
		delete(h.offers, id)
		return
	}
	h.offers[id] = holder{instance: instance}
}

// release frees the offer instance was rented from, once the instance is destroyed.
func (h *held) release(instance string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, holder := range h.offers {
		if holder.instance == instance {
			delete(h.offers, id)
		}
	}
}

// keep is told the account's instances after each read of them, and frees the offer of one that
// was listed before and is gone: an instance the vendor removed frees its offer without a
// Destroy. One not listed yet keeps its offer, since the list trails a rent.
func (h *held) keep(onAccount map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, holder := range h.offers {
		switch {
		case holder.instance == "":
		case onAccount[holder.instance]:
			holder.listed = true
			h.offers[id] = holder
		case holder.listed:
			delete(h.offers, id)
		}
	}
}

// free returns the offers nobody in this process has.
func (h *held) free(offers []offer) []offer {
	h.mu.Lock()
	defer h.mu.Unlock()
	var open []offer
	for _, candidate := range offers {
		if _, taken := h.offers[candidate.ID]; !taken {
			open = append(open, candidate)
		}
	}
	return open
}

// candidates returns the offers one Create tries, in the order it tries them: the cheapest offer
// of each of the offerCandidates cheapest machines that fit the spec and that this process does
// not already hold, shuffled.
//
// Always taking the cheapest offer sent every retry of a job to the same machine, and a machine
// that cannot start a container stays the cheapest because nobody keeps it. Nothing the vendor
// shows before renting tells such a machine apart (the one met was verified, with a reliability
// of 0.998), and the adapter remembers no machine's failures, so the choice is spread over a few
// machines. A retry is then unlikely to meet the same machine again; it still can, unless the
// caller names the machine in the spec's AvoidHosts, which the search leaves out.
func (p *Provider) candidates(ctx context.Context, spec provider.Spec) ([]offer, error) {
	query, err := p.offerQuery(spec)
	if err != nil {
		return nil, err
	}
	found, _, err := p.offers(ctx, query, offerSearchLimit, p.marketFor)
	if errors.Is(err, errSearchBudget) {
		return nil, fmt.Errorf("vast: %w %q: %w", provider.ErrNoOffer, spec.Name, err)
	}
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("vast: %w %q within the configured filters", provider.ErrNoOffer, spec.Name)
	}
	open := p.held.free(found)
	if len(open) == 0 {
		return nil, fmt.Errorf("vast: %w %q: this agent's own instances hold all %d offers within the configured filters", provider.ErrNoOffer, spec.Name, len(found))
	}
	chosen := cheapestPerMachine(open, offerCandidates)
	p.shuffle(chosen)
	return chosen, nil
}

var _ provider.Market = (*Provider)(nil)

const (
	// marketInterval is how long one read of an offer search stands.
	marketInterval = 5 * time.Minute
	// shortfallInterval is how long a read of the offers of a size alone stands. It only words
	// why a size has nothing to rent, which changes slowly.
	shortfallInterval = time.Hour
	// dailySearchRows is the most offer rows the adapter asks for in a UTC day. The vendor's quota
	// is 20000 rows a day and counts every row; a restart forgets what was spent, so this stays
	// below it.
	dailySearchRows = 15000
)

var errSearchBudget = errors.New("the day's offer search budget is spent")

// searches is the one place offers are searched from: the last read of each search, by the
// search without its limit, and the rows the day's reads were answered with, as the vendor counts
// them. A search books its whole limit until it is answered. Each read has its own lock,
// so a caller waits on the vendor only for a search another caller is making.
type searches struct {
	mu     sync.Mutex
	reads  map[string]*searchRead
	day    string
	asked  int
	budget int
	warned string
}

// searchRead is one search's offers. One that failed is not kept.
type searchRead struct {
	mu    sync.Mutex
	at    time.Time
	asked int
	found []offer
}

func (s *searches) of(key string) *searchRead {
	s.mu.Lock()
	defer s.mu.Unlock()
	read, ok := s.reads[key]
	if !ok {
		read = &searchRead{}
		s.reads[key] = read
	}
	return read
}

// offers is what the read last found.
func (r *searchRead) offers() []offer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.found
}

func (s *searches) forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reads, key)
}

// take books rows of the day's budget, or says it has no room for them. firstRefusal is true
// for the day's first refusal.
func (s *searches) take(rows int) (firstRefusal bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if today := time.Now().UTC().Format(time.DateOnly); s.day != today {
		s.day, s.asked = today, 0
	}
	if s.asked+rows > s.budget {
		first := s.warned != s.day
		s.warned = s.day
		return first, fmt.Errorf("vast: %w: %d of %d rows read today, and a search for %d more is not made until 00:00 UTC", errSearchBudget, s.asked, s.budget, rows)
	}
	s.asked += rows
	return false, nil
}

// refund gives back rows booked for a search that did not answer with them.
func (s *searches) refund(rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = max(0, s.asked-rows)
}

func searchKey(query map[string]any) (string, error) {
	unlimited := maps.Clone(query)
	delete(unlimited, "limit")
	key, err := json.Marshal(unlimited)
	if err != nil {
		return "", fmt.Errorf("vast: key a search: %w", err)
	}
	return string(key), nil
}

// offers answers query for up to limit offers from its last read while that is younger than
// standFor and asked for as many or found fewer than it asked, and from a new read within the
// day's budget otherwise. fresh says the answer is a new read.
func (p *Provider) offers(ctx context.Context, query map[string]any, limit int, standFor time.Duration) (found []offer, fresh bool, err error) {
	key, err := searchKey(query)
	if err != nil {
		return nil, false, err
	}
	read := p.searches.of(key)
	read.mu.Lock()
	defer read.mu.Unlock()
	if !read.at.IsZero() && time.Since(read.at) < standFor && (limit <= read.asked || len(read.found) < read.asked) {
		return read.found, false, nil
	}
	first, err := p.searches.take(limit)
	if first {
		p.log("offer searches stop until 00:00 UTC: %v", err)
	}
	if err != nil {
		return nil, false, err
	}
	limited := maps.Clone(query)
	limited["limit"] = limit
	found, err = p.search(ctx, limited)
	if err != nil {
		p.searches.refund(limit)
		return nil, false, err
	}
	p.searches.refund(limit - len(found))
	read.at, read.asked, read.found = time.Now(), limit, found
	return found, true, nil
}

// forgetOffers drops the read of spec's offers once every one Create tried was taken, so the
// next Create searches again.
func (p *Provider) forgetOffers(spec provider.Spec) {
	query, err := p.offerQuery(spec)
	if err != nil {
		return
	}
	if key, err := searchKey(query); err == nil {
		p.searches.forget(key)
	}
}

// Rentable counts the offers candidates chooses among: the same search, asked for as many offers
// as are wanted on top of the ones this process holds that its last read listed, as the search
// goes on listing them. When
// none passes the filters, the offers of the spec's size alone say which filters leave them out.
// Both come through offers, so a read stands for a while and the day's budget bounds them.
func (p *Provider) Rentable(ctx context.Context, spec provider.Spec, atMost int) (provider.Supply, error) {
	query, err := p.offerQuery(spec)
	if err != nil {
		return provider.Supply{}, err
	}
	key, err := searchKey(query)
	if err != nil {
		return provider.Supply{}, err
	}
	previous := p.searches.of(key).offers()
	heldHere := len(previous) - len(p.held.free(previous))
	found, _, err := p.offers(ctx, query, atMost+heldHere, p.marketFor)
	if err != nil {
		return provider.Supply{}, err
	}
	if open := p.held.free(found); len(open) > 0 {
		return provider.Supply{Count: min(atMost, len(open))}, nil
	}
	if len(found) > 0 {
		return provider.Supply{Why: "this agent's own instances hold every vast offer within the filters"}, nil
	}
	return p.shortfall(ctx, spec)
}

// shortfall says why no offer of spec's size passes the filters: which of them leave out the
// offers there are. A new read logs how many each leaves out, as one offer may fail several.
func (p *Provider) shortfall(ctx context.Context, spec provider.Spec) (provider.Supply, error) {
	query, err := p.sizeQuery(spec)
	if err != nil {
		return provider.Supply{}, err
	}
	sized, fresh, err := p.offers(ctx, query, sizeSearchLimit, shortfallInterval)
	if err != nil {
		return provider.Supply{}, err
	}
	log := func(format string, args ...any) {
		if fresh {
			p.log(format, args...)
		}
	}
	if len(sized) == 0 {
		why := "vast has no on-demand offer of its size (" + describeSize(spec, p.cfg) + ")"
		log("nothing to rent: %s", why)
		return provider.Supply{Why: why}, nil
	}
	filters := p.cfg.filters()
	var failing, counts []string
	for _, f := range filters {
		out := 0
		for _, o := range sized {
			if !f.passes(o) {
				out++
			}
		}
		if out > 0 {
			failing = append(failing, f.name)
			counts = append(counts, fmt.Sprintf("%s %d", f.name, out))
		}
	}
	detail := fmt.Sprintf("%d offers of the size, the cheapest at $%.3f/h", len(sized), sized[0].PricePerHour)
	if len(counts) > 0 {
		detail += "; left out by " + strings.Join(counts, ", ")
	}
	why := "no vast offer of its size passed the filters when searched"
	if len(failing) > 0 {
		why = "no vast offer of its size passes " + strings.Join(failing, ", ")
	}
	log("nothing to rent of %s: %s (%s)", describeSize(spec, p.cfg), why, detail)
	return provider.Supply{Why: why}, nil
}

func (p *Provider) search(ctx context.Context, query map[string]any) ([]offer, error) {
	var found struct {
		Offers []offer `json:"offers"`
	}
	if _, err := p.call(ctx, http.MethodPost, "/api/v0/bundles/", query, &found); err != nil {
		return nil, err
	}
	return found.Offers, nil
}

// filter is one of the settings' filters as offerQuery applies it, by the setting's name.
type filter struct {
	name   string
	passes func(offer) bool
}

func (c Settings) filters() []filter {
	excluded := map[int64]bool{}
	for _, machine := range c.ExcludedMachines {
		excluded[machine] = true
	}
	regions := map[string]bool{}
	for _, region := range c.Regions {
		regions[strings.ToUpper(region)] = true
	}
	return []filter{
		{"regions", func(o offer) bool { return len(regions) == 0 || regions[country(o.Geolocation)] }},
		{"max_price_per_hour", func(o offer) bool { return o.PricePerHour <= c.MaxPricePerHour }},
		{"min_reliability", func(o offer) bool { return o.Reliability >= c.MinReliability }},
		{"verified_only", func(o offer) bool { return !c.VerifiedOnly || o.Verification == "verified" }},
		{"excluded_machines", func(o offer) bool { return !excluded[o.MachineID] }},
	}
}

// country is the country code a geolocation ends in.
func country(geolocation string) string {
	parts := strings.Split(geolocation, ",")
	return strings.ToUpper(strings.TrimSpace(parts[len(parts)-1]))
}

func describeSize(spec provider.Spec, c Settings) string {
	gpu := "any GPU"
	if spec.AcceleratorCount > 0 {
		gpu = fmt.Sprintf("%d x %s", spec.AcceleratorCount, spec.AcceleratorType)
		if accelerator, err := c.accelerator(spec.AcceleratorType); err == nil {
			gpu = fmt.Sprintf("%d x %s", spec.AcceleratorCount, accelerator.GPUName)
		}
	}
	return fmt.Sprintf("%s, %s, %d cores, %d MB memory, %d GB disk", gpu, spec.Architecture, provider.CeilDiv(spec.CPUMillis, 1000), provider.CeilDiv(spec.MemoryBytes, bytesPerMB), diskGB(spec))
}

// cheapestPerMachine keeps the first offer of each machine, up to limit machines. offers come
// cheapest first.
func cheapestPerMachine(offers []offer, limit int) []offer {
	seen := map[int64]bool{}
	var kept []offer
	for _, candidate := range offers {
		if seen[candidate.MachineID] {
			continue
		}
		seen[candidate.MachineID] = true
		kept = append(kept, candidate)
		if len(kept) == limit {
			break
		}
	}
	return kept
}

func shuffleOffers(offers []offer) {
	rand.Shuffle(len(offers), func(i, j int) { offers[i], offers[j] = offers[j], offers[i] })
}

func diskGB(spec provider.Spec) int64 {
	return provider.CeilDiv(spec.DiskBytes, bytesPerGB)
}
