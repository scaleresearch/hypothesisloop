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
}

type Provider struct {
	mu        sync.Mutex
	created   int
	instances map[string]*instance
	calls     map[string]int

	// Fail, when set, is asked before every call and its error returned in place of the call.
	// method is "Create", "List", "Logs", "Stop" or "Destroy"; name is the instance's, empty for
	// List.
	Fail func(method, name string) error
}

func New() *Provider {
	return &Provider{instances: map[string]*instance{}, calls: map[string]int{}}
}

var _ provider.Provider = (*Provider)(nil)

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
		Instance: provider.Instance{ID: id, Name: spec.Name, State: provider.StateStarting},
		spec:     spec,
	}
	return id, nil
}

func (p *Provider) List(_ context.Context, namePrefix string) ([]provider.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fail("List", ""); err != nil {
		return nil, err
	}
	var out []provider.Instance
	for _, i := range p.instances {
		if strings.HasPrefix(i.Name, namePrefix) {
			out = append(out, i.Instance)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
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

// Start moves every instance of the name to StateRunning.
func (p *Provider) Start(name string) {
	p.each(name, func(i *instance) {
		i.State = provider.StateRunning
		i.StartedAt = time.Now()
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
