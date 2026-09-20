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

	// Fail, when set, is asked before every call and its error returned in place of the call.
	// method is "Create", "List", "Logs", "Stop" or "Destroy"; name is the instance's, empty for
	// List.
	Fail func(method, name string) error
}

func New() *Provider {
	return &Provider{instances: map[string]*instance{}}
}

var _ provider.Provider = (*Provider)(nil)

func (p *Provider) Create(_ context.Context, spec provider.Spec) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.fail("Create", spec.Name); err != nil {
		return "", err
	}
	if spec.Name == "" {
		return "", fmt.Errorf("fake: instance has no name")
	}
	for _, existing := range p.instances {
		if existing.Name == spec.Name {
			return "", fmt.Errorf("fake: instance %q already exists", spec.Name)
		}
	}
	p.created++
	id := fmt.Sprintf("fake-%d", p.created)
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
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
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

// Start moves the named instance to StateRunning.
func (p *Provider) Start(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.named(name)
	i.State = provider.StateRunning
	i.StartedAt = time.Now()
}

// Print appends lines to the named instance's output.
func (p *Provider) Print(name string, lines ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.named(name)
	i.logs = append(i.logs, lines...)
}

// Exit moves the named instance to StateExited after it wrote lines.
func (p *Provider) Exit(name string, lines ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.named(name)
	i.logs = append(i.logs, lines...)
	i.State = provider.StateExited
}

// Spec is what the named instance was created from.
func (p *Provider) Spec(name string) provider.Spec {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.named(name).spec
}

// Stops is how many times the named instance was sent SIGTERM.
func (p *Provider) Stops(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.named(name).stops
}

// Names lists every instance that exists, sorted.
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

func (p *Provider) fail(method, name string) error {
	if p.Fail == nil {
		return nil
	}
	return p.Fail(method, name)
}

func (p *Provider) named(name string) *instance {
	for _, i := range p.instances {
		if i.Name == name {
			return i
		}
	}
	panic(fmt.Sprintf("fake: no instance named %q", name))
}
