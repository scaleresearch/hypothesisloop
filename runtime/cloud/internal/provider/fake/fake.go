// Package fake is an in-memory provider.Provider for tests of whatever is built on the contract.
// A test moves an instance through its life with Start and Exit, which is what a vendor does on
// its own schedule.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

type instance struct {
	provider.Instance
	spec  provider.Spec
	logs  []string
	stops int
	// warm is an instance Warm rented and StartWarm has not started.
	warm bool
	// hidden is an instance List leaves out while Get still finds it, as a vendor whose list
	// trails its instances does.
	hidden bool
}

type Provider struct {
	mu        sync.Mutex
	created   int
	instances map[string]*instance
	calls     map[string]int

	// Fail, when set, is asked before every call and its error returned in place of the call.
	// method is "Create", "List", "Logs", "Stop", "Destroy", "Pause", "Resume", "Rentable", "Warm",
	// "StartWarm" or "Get"; name is the instance's, empty for List, Rentable and an id Get does not
	// find, and for StartWarm the
	// name it is started under.
	Fail func(method, name string) error

	// Market, when set, is what the vendor has to rent of a spec right now. Unset, there is one
	// for every instance asked about.
	Market func(spec provider.Spec) provider.Supply
}

func New() *Provider {
	return &Provider{instances: map[string]*instance{}, calls: map[string]int{}}
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Pauser   = (*Provider)(nil)
	_ provider.Market   = (*Provider)(nil)
	_ provider.Warmer   = (*Provider)(nil)
)

func (p *Provider) Rentable(_ context.Context, spec provider.Spec, atMost int) (provider.Supply, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fail("Rentable", ""); err != nil {
		return provider.Supply{}, err
	}
	if p.Market == nil {
		return provider.Supply{Count: atMost}, nil
	}
	supply := p.Market(spec)
	supply.Count = max(0, min(atMost, supply.Count))
	return supply, nil
}

func (p *Provider) Create(_ context.Context, spec provider.Spec) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fail("Create", spec.Name); err != nil {
		return "", err
	}
	if err := provider.ValidateName(spec.Name); err != nil {
		return "", fmt.Errorf("fake: %w", err)
	}
	p.created++
	id := fmt.Sprintf("fake-%04d", p.created)
	p.instances[id] = &instance{
		Instance: provider.Instance{ID: id, Name: spec.Name, State: provider.StateStarting, StartedAt: time.Now()},
		spec:     spec,
	}
	return id, nil
}

// Warm rents an instance that is starting until Ready makes it warm.
func (p *Provider) Warm(_ context.Context, spec provider.Spec) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fail("Warm", spec.Name); err != nil {
		return "", err
	}
	if err := provider.ValidateName(spec.Name); err != nil {
		return "", fmt.Errorf("fake: %w", err)
	}
	p.created++
	id := fmt.Sprintf("fake-%04d", p.created)
	p.instances[id] = &instance{
		Instance: provider.Instance{ID: id, Name: spec.Name, State: provider.StateStarting, StartedAt: time.Now()},
		spec:     provider.Spec{Name: spec.Name, Image: spec.Image, CPUMillis: spec.CPUMillis, MemoryBytes: spec.MemoryBytes, DiskBytes: spec.DiskBytes},
		warm:     true,
	}
	return id, nil
}

// StartWarm refuses an instance that is not warm, as a vendor would one that runs or is gone.
func (p *Provider) StartWarm(_ context.Context, id string, spec provider.Spec) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fail("StartWarm", spec.Name); err != nil {
		return err
	}
	i, ok := p.instances[id]
	if !ok {
		return fmt.Errorf("fake: no instance %q to start", id)
	}
	if i.State != provider.StateWarm {
		return fmt.Errorf("fake: instance %q is %s, and only a warm instance is started", id, i.State)
	}
	if i.spec.Image != spec.Image || i.spec.CPUMillis != spec.CPUMillis || i.spec.MemoryBytes != spec.MemoryBytes || i.spec.DiskBytes != spec.DiskBytes {
		return fmt.Errorf("fake: instance %q was warmed for another image or size", id)
	}
	i.Name, i.spec, i.State, i.warm, i.StartedAt = spec.Name, spec, provider.StateStarting, false, time.Now()
	return nil
}

// Ready moves every warmed instance of the name that is still starting to StateWarm.
func (p *Provider) Ready(name string) {
	p.each(name, func(i *instance) {
		if i.warm && i.State == provider.StateStarting {
			i.State = provider.StateWarm
		}
	})
}

func (p *Provider) List(_ context.Context, namePrefix string) ([]provider.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fail("List", ""); err != nil {
		return nil, err
	}
	var out []provider.Instance
	for _, i := range p.instances {
		if strings.HasPrefix(i.Name, namePrefix) && !i.hidden {
			out = append(out, i.Instance)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (p *Provider) Get(_ context.Context, id string) (provider.Instance, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.instances[id]
	name := ""
	if ok {
		name = i.Name
	}
	if err := p.fail("Get", name); err != nil {
		return provider.Instance{}, false, err
	}
	if !ok {
		return provider.Instance{}, false, nil
	}
	return i.Instance, true, nil
}

// Hide leaves every instance of the name out of List, and Show puts it back. Get finds it
// either way.
func (p *Provider) Hide(name string) {
	p.each(name, func(i *instance) { i.hidden = true })
}

func (p *Provider) Show(name string) {
	p.each(name, func(i *instance) { i.hidden = false })
}

func (p *Provider) Logs(_ context.Context, id string, tail int) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.instances[id]
	if !ok {
		return nil, fmt.Errorf("fake: no instance %q", id)
	}
	if err := p.fail("Logs", i.Name); err != nil {
		return nil, err
	}
	lines := i.logs
	if tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	return append([]string(nil), lines...), nil
}

func (p *Provider) Stop(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.instances[id]
	if !ok {
		return nil
	}
	if err := p.fail("Stop", i.Name); err != nil {
		return err
	}
	i.stops++
	return nil
}

func (p *Provider) Destroy(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.instances[id]
	if !ok {
		return nil
	}
	if err := p.fail("Destroy", i.Name); err != nil {
		return err
	}
	delete(p.instances, id)
	return nil
}

// Pause keeps the command where it is: Resume continues the same process.
func (p *Provider) Pause(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.instances[id]
	if !ok {
		return nil
	}
	if err := p.fail("Pause", i.Name); err != nil {
		return err
	}
	switch i.State {
	case provider.StateRunning:
		i.State = provider.StatePaused
	case provider.StatePaused:
	default:
		return fmt.Errorf("fake: instance %q is %s, and only a running instance can be paused", id, i.State)
	}
	return nil
}

func (p *Provider) Resume(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	i, ok := p.instances[id]
	if !ok {
		return fmt.Errorf("fake: no instance %q to resume", id)
	}
	if err := p.fail("Resume", i.Name); err != nil {
		return err
	}
	if i.State == provider.StatePaused {
		i.State = provider.StateRunning
	}
	return nil
}

// Start moves every instance of the name to StateRunning.
func (p *Provider) Start(name string) {
	p.each(name, func(i *instance) {
		i.State = provider.StateRunning
		i.StartedAt = time.Now()
	})
}

// Age moves the start of every instance of the name back by d.
func (p *Provider) Age(name string, d time.Duration) {
	p.each(name, func(i *instance) { i.StartedAt = i.StartedAt.Add(-d) })
}

// Lose moves every instance of the name to StateUnknown, as a vendor that cannot reach its host
// lists it.
func (p *Provider) Lose(name string) {
	p.each(name, func(i *instance) { i.State = provider.StateUnknown })
}

// RunsOn sets what every instance of the name runs on, as a vendor that says so lists it.
func (p *Provider) RunsOn(name, host, acceleratorModel, cpuModel string, pricePerHour float64) {
	p.each(name, func(i *instance) {
		i.Host, i.AcceleratorModel, i.CPUModel, i.PricePerHour = host, acceleratorModel, cpuModel, pricePerHour
	})
}

// Print appends lines to the output of every instance of the name.
func (p *Provider) Print(name string, lines ...string) {
	p.each(name, func(i *instance) { i.logs = append(i.logs, lines...) })
}

// Exit moves every instance of the name to StateExited after it wrote lines.
func (p *Provider) Exit(name string, lines ...string) {
	p.each(name, func(i *instance) {
		i.logs = append(i.logs, lines...)
		i.State = provider.StateExited
	})
}

// Expire moves every instance of the name to StateExited with Expired set, as a vendor whose own
// lifetime ended it lists it, after it wrote lines.
func (p *Provider) Expire(name string, lines ...string) {
	p.each(name, func(i *instance) {
		i.logs = append(i.logs, lines...)
		i.State, i.Expired = provider.StateExited, true
	})
}

// Spec is what the first instance of the name was created from.
func (p *Provider) Spec(name string) provider.Spec {
	var spec provider.Spec
	found := false
	p.each(name, func(i *instance) {
		if !found {
			spec, found = i.spec, true
		}
	})
	return spec
}

// Stops is how many times instances of the name were sent SIGTERM.
func (p *Provider) Stops(name string) int {
	stops := 0
	p.each(name, func(i *instance) { stops += i.stops })
	return stops
}

// Names lists every instance that exists, sorted. A name rented twice is listed twice.
func (p *Provider) Names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.instances))
	for _, i := range p.instances {
		names = append(names, i.Name)
	}
	sort.Strings(names)
	return names
}

// IDs lists the ids of the instances of the name, sorted.
func (p *Provider) IDs(name string) []string {
	var ids []string
	p.each(name, func(i *instance) { ids = append(ids, i.ID) })
	return ids
}

// Calls is how many times method was called, failed calls included.
func (p *Provider) Calls(method string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[method]
}

func (p *Provider) fail(method, name string) error {
	p.calls[method]++
	if p.Fail == nil {
		return nil
	}
	return p.Fail(method, name)
}

// each runs do on the instances of the name in id order, and panics when there is none: a test
// that names an instance that does not exist is wrong.
func (p *Provider) each(name string, do func(*instance)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var matched []*instance
	for _, i := range p.instances {
		if i.Name == name {
			matched = append(matched, i)
		}
	}
	if len(matched) == 0 {
		panic(fmt.Sprintf("fake: no instance named %q", name))
	}
	sort.Slice(matched, func(a, b int) bool { return matched[a].ID < matched[b].ID })
	for _, i := range matched {
		do(i)
	}
}
