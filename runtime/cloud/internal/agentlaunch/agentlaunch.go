// Package agentlaunch converges one provider account onto the agent instances the control plane
// says should exist. It names no vendor and keeps nothing between passes: every pass asks the
// control plane for its list and the provider for what is rented, and acts on the difference.
package agentlaunch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// Size is what every agent instance is rented at.
type Size struct {
	CPUMillis   int64
	MemoryBytes int64
	DiskBytes   int64
}

type Config struct {
	// Name is the launcher the control plane knows this process as.
	Name   string
	APIURL string
	// Token is this launcher's own token, sent on every call to the control plane.
	Token string
	// Namespace is part of every instance name. The launcher lists, creates and destroys the
	// instances of its namespace and no others.
	Namespace string
	// Command is the argv every instance runs.
	Command []string
	Size    Size
	// Env is added to every instance's environment here, so what it holds never passes through
	// the control plane. The control plane's list may not set any of its names.
	Env        map[string]string
	Provider   provider.Provider
	HTTPClient *http.Client
	Interval   time.Duration
	// BootLimit is how long an instance may stay in StateStarting, counted from the StartedAt its
	// provider reports, before it is destroyed and rented again.
	BootLimit time.Duration
	// CreatePause is waited between two creates of one pass, for a provider that limits how fast
	// instances may be rented. Zero is no pause.
	CreatePause time.Duration
	Log         func(format string, args ...any)
}

type Launcher struct {
	cfg      Config
	prefix   string
	provider provider.Provider
}

func New(cfg Config) (*Launcher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Token == "" || cfg.Provider == nil || cfg.HTTPClient == nil || cfg.Log == nil {
		return nil, errors.New("agentlaunch: Token, Provider, HTTPClient and Log are required")
	}
	return &Launcher{cfg: cfg, prefix: provider.AgentPrefix(cfg.Namespace), provider: provider.Bounded(cfg.Provider)}, nil
}

// Validate checks what a config file decides, so the file is refused when it is loaded, before
// the provider is built or the control plane contacted.
func (cfg Config) Validate() error {
	if cfg.Name == "" || cfg.APIURL == "" {
		return errors.New("agentlaunch: Name and APIURL are required")
	}
	if err := provider.ValidateSegment("namespace", cfg.Namespace); err != nil {
		return fmt.Errorf("agentlaunch: %w", err)
	}
	if len(cfg.Command) == 0 {
		return errors.New("agentlaunch: Command is required")
	}
	if cfg.Size.CPUMillis <= 0 || cfg.Size.MemoryBytes <= 0 || cfg.Size.DiskBytes <= 0 {
		return errors.New("agentlaunch: the instance size needs positive cpu, memory and storage")
	}
	if cfg.Interval <= 0 {
		return errors.New("agentlaunch: Interval must be positive")
	}
	if cfg.BootLimit <= 0 {
		return errors.New("agentlaunch: BootLimit must be positive")
	}
	if cfg.CreatePause < 0 {
		return errors.New("agentlaunch: CreatePause must not be negative")
	}
	for name, value := range cfg.Env {
		if name == "" || value == "" {
			return fmt.Errorf("agentlaunch: Env entry %q needs a name and a value", name)
		}
	}
	return nil
}

// Run passes until ctx is cancelled. A pass that fails is logged and the next one starts from
// nothing, like every other.
func (l *Launcher) Run(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.Interval)
	defer ticker.Stop()
	for {
		if err := l.Pass(ctx); err != nil {
			l.cfg.Log("pass: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Pass is one convergence. The desired list is read before anything is touched, so a control
// plane that cannot be reached or answers with anything but a complete list changes nothing.
// After that every instance is handled by itself: one that cannot be created or destroyed is
// reported in the returned error and the rest of the pass still runs.
func (l *Launcher) Pass(ctx context.Context) error {
	want, err := l.fetchDesired(ctx)
	if err != nil {
		return fmt.Errorf("fetch desired state: %w", err)
	}
	rented, err := l.provider.List(ctx, l.prefix)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	byName := map[string][]provider.Instance{}
	for _, i := range rented {
		byName[i.Name] = append(byName[i.Name], i)
	}

	now := time.Now()
	var failures []error
	destroy := func(i provider.Instance, why string) bool {
		if err := l.provider.Destroy(ctx, i.ID); err != nil {
			l.cfg.Log("destroy %s (%s): %v", i.Name, why, err)
			failures = append(failures, fmt.Errorf("destroy %s: %w", i.Name, err))
			return false
		}
		l.cfg.Log("destroyed %s (%s)", i.Name, why)
		return true
	}

	// Everything that goes is destroyed before anything is created, so an agent whose instance
	// is replaced under a new name runs twice for as short a time as the provider allows.
	for _, name := range sortedKeys(byName) {
		if _, wanted := want.instances[name]; !wanted && !want.ending[name] {
			for _, i := range byName[name] {
				destroy(i, "no longer desired")
			}
		}
	}

	var creates, failedCreates int
	var lastCreateError error
	for _, name := range sortedKeys(want.instances) {
		// One instance runs the agent: the live one with the lowest id. A provider that lists
		// with a lag or a create that failed after it succeeded leaves a second, an exited one
		// is a harness that crashed, and one starting for longer than the boot limit will never
		// run its command. All three go, and a name left with nothing live is rented again,
		// unless something of it could not be destroyed: then the next pass decides.
		kept, cleared := false, true
		instances := byName[name]
		sort.SliceStable(instances, func(a, b int) bool { return instances[a].ID < instances[b].ID })
		for _, i := range instances {
			switch {
			case i.State == provider.StateExited:
				cleared = destroy(i, "its command exited") && cleared
			case l.stuckStarting(i, now):
				cleared = destroy(i, fmt.Sprintf("still starting after %s, the boot limit is %s", now.Sub(i.StartedAt).Round(time.Second), l.cfg.BootLimit)) && cleared
			case kept:
				cleared = destroy(i, "duplicate") && cleared
			default:
				kept = true
			}
		}
		if kept || !cleared {
			continue
		}
		if creates > 0 && !l.pause(ctx) {
			failures = append(failures, fmt.Errorf("create %s: %w", name, ctx.Err()))
			break
		}
		creates++
		if _, err := l.provider.Create(ctx, l.spec(want.instances[name])); err != nil {
			l.cfg.Log("create %s: %v", name, err)
			failures = append(failures, fmt.Errorf("create %s: %w", name, err))
			failedCreates++
			lastCreateError = err
			continue
		}
		l.cfg.Log("created %s", name)
	}
	// Every create failing is the account, the image or the size in this launcher's config, and
	// nothing will run until it is changed, so it is said once more in a line of its own.
	if creates > 0 && failedCreates == creates {
		l.cfg.Log("NO AGENT COULD BE STARTED: all %d creates of this pass failed, the last with: %v", creates, lastCreateError)
	}
	return errors.Join(failures...)
}

// stuckStarting reports an instance that has been starting for longer than the boot limit. An
// instance whose provider reports no start time, or one in the future, has no age to judge and
// is left to start.
func (l *Launcher) stuckStarting(i provider.Instance, now time.Time) bool {
	if i.State != provider.StateStarting || i.StartedAt.Unix() <= 0 {
		return false
	}
	return now.Sub(i.StartedAt) > l.cfg.BootLimit
}

// pause waits CreatePause and reports false when ctx ended first.
func (l *Launcher) pause(ctx context.Context) bool {
	if l.cfg.CreatePause == 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(l.cfg.CreatePause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (l *Launcher) spec(i instance) provider.Spec {
	env := make(map[string]string, len(i.env)+len(l.cfg.Env))
	for name, value := range i.env {
		env[name] = value
	}
	for name, value := range l.cfg.Env {
		env[name] = value
	}
	return provider.Spec{
		Name:        i.name,
		Image:       i.image,
		Command:     append([]string(nil), l.cfg.Command...),
		Env:         env,
		CPUMillis:   l.cfg.Size.CPUMillis,
		MemoryBytes: l.cfg.Size.MemoryBytes,
		DiskBytes:   l.cfg.Size.DiskBytes,
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
