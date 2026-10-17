package agentsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	sandboxapi "github.com/scaleresearch/hypothesisloop/contract/agentsandbox"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider"
)

// Size is what every agent instance is rented at.
type Size struct {
	CPUMillis   int64
	MemoryBytes int64
	DiskBytes   int64
}

// Platform is what the launcher asks of the platform: the machines it holds, and the machines
// to keep for what the launcher observed (service/agentsandbox.Decider).
type Platform interface {
	Held(ctx context.Context) ([]sandboxapi.HeldMachine, error)
	Reconcile(ctx context.Context, observed sandboxapi.Observed) (*sandboxapi.Desired, error)
}

type Config struct {
	// Name is part of every instance name: the launcher lists, creates and destroys the instances
	// that start with it and no others, so two deployments on one provider account keep apart by
	// their names.
	Name string
	// Command is the argv every instance runs.
	Command []string
	Size    Size
	// Provider rents the instances. An instance's environment is the platform's alone: where it
	// is reached and the agent's own token. No model credential is in it, as the agents' model
	// calls go through the control plane, which holds that.
	Provider provider.Provider
	Platform Platform
	Interval time.Duration
	// CreatePause is waited between two rentals of one pass (a create or a warm), for a provider
	// that limits how fast instances may be rented. Zero is no pause. Starting a warm instance is
	// no rental and is never paused for.
	CreatePause time.Duration
	// WarmPool is how many warm instances the launcher keeps ready to start in place of a create
	// (pool.go). Zero keeps none and destroys any there are. Above zero the provider must be a
	// provider.Warmer.
	WarmPool int
	Log      func(format string, args ...any)
}

type Launcher struct {
	cfg          Config
	prefix       string
	poolPrefix   string
	provider     provider.Provider
	warmer       provider.Warmer
	launchDigest string
}

func New(cfg Config) (*Launcher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Provider == nil || cfg.Platform == nil || cfg.Log == nil {
		return nil, errors.New("agentsandbox: Provider, Platform and Log are required")
	}
	digest, err := cfg.launchDigest()
	if err != nil {
		return nil, err
	}
	l := &Launcher{
		cfg: cfg, prefix: provider.AgentPrefix(cfg.Name), provider: provider.Bounded(cfg.Provider),
		launchDigest: digest,
	}
	l.poolPrefix = l.prefix + poolSegment
	l.warmer, _ = l.provider.(provider.Warmer)
	if cfg.WarmPool > 0 && l.warmer == nil {
		return nil, fmt.Errorf("agentsandbox: a warm pool of %d needs a provider that can warm instances, and this one can %v", cfg.WarmPool, provider.Capabilities(cfg.Provider))
	}
	return l, nil
}

// launchDigest is a digest of everything this launcher itself starts an instance with. The
// control plane makes it part of each instance's name, so a changed command or size replaces the
// running instances as a changed image does.
func (cfg Config) launchDigest() (string, error) {
	inputs, err := json.Marshal(struct {
		Command []string `json:"command"`
		Size    Size     `json:"size"`
	}{cfg.Command, cfg.Size})
	if err != nil {
		return "", fmt.Errorf("agentsandbox: digest launch inputs: %w", err)
	}
	digest := sha256.Sum256(inputs)
	return hex.EncodeToString(digest[:]), nil
}

// Validate checks what the settings decide, so they are refused when they are loaded, before
// the provider is built.
func (cfg Config) Validate() error {
	if err := provider.ValidateSegment("sandbox name", cfg.Name); err != nil {
		return fmt.Errorf("agentsandbox: %w", err)
	}
	if len(cfg.Command) == 0 {
		return errors.New("agentsandbox: Command is required")
	}
	if cfg.Size.CPUMillis <= 0 || cfg.Size.MemoryBytes <= 0 || cfg.Size.DiskBytes <= 0 {
		return errors.New("agentsandbox: the instance size needs positive cpu, memory and storage")
	}
	if cfg.Interval <= 0 {
		return errors.New("agentsandbox: Interval must be positive")
	}
	if cfg.CreatePause < 0 || cfg.WarmPool < 0 {
		return errors.New("agentsandbox: CreatePause and WarmPool must not be negative")
	}
	return nil
}

// Run passes until ctx is cancelled. A pass that fails is logged and the next one starts from
// nothing, like every other.
//
// Once ctx is cancelled the warm pool is destroyed, and the agents' instances are left to the
// next leader.
func (l *Launcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(l.cfg.Interval)
	defer ticker.Stop()
	for {
		if err := l.Pass(ctx); err != nil {
			l.cfg.Log("pass: %v", err)
		}
		select {
		case <-ctx.Done():
			l.drainPool(context.WithoutCancel(ctx))
			return nil
		case <-ticker.C:
		}
	}
}

// Pass is one convergence. The provider is listed and the platform told what was found before
// anything is touched, so a provider that cannot be listed, or a platform that cannot answer,
// changes nothing. A machine the
// platform holds that the list leaves out, an agent's or the warm pool's, is read by its id first
// (confirmUnlisted), and the pool found is reported for the platform to hold. After that every
// instance is handled by itself: one that cannot be created, stopped or destroyed is reported in
// the returned error and the rest of the pass still runs.
//
// What is no longer desired is destroyed first, and what is missing is created after, from the
// warm pool where it has an instance ready: an agent whose instance is replaced under a new name
// never runs twice, as its old instance is gone before the new one's command starts. One whose
// destroy failed is destroyed again on the next pass.
//
// An answer that states a refusal is acted on like any other, except that nothing is created,
// and the pass returns ErrRefused with the control plane's words.
func (l *Launcher) Pass(ctx context.Context) error {
	held, err := l.cfg.Platform.Held(ctx)
	if err != nil {
		return fmt.Errorf("fetch the machines the platform holds: %w", err)
	}
	rented, err := l.provider.List(ctx, l.prefix)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	if rented, err = l.confirmUnlisted(ctx, rented, held); err != nil {
		return err
	}
	// The launcher pauses nothing yet (see the provider package's comment on capabilities).
	rented = provider.PausedAsStarting(rented)
	byName := map[string][]provider.Instance{}
	var pool []provider.Instance
	for _, i := range rented {
		if strings.HasPrefix(i.Name, l.poolPrefix) {
			pool = append(pool, i)
			continue
		}
		byName[i.Name] = append(byName[i.Name], i)
	}
	observed := sandboxapi.Observed{LaunchDigest: l.launchDigest, Instances: l.observe(ctx, byName), Pool: []sandboxapi.PoolMachine{}}
	for _, i := range sortedByID(pool) {
		observed.Pool = append(observed.Pool, sandboxapi.PoolMachine{Name: i.Name[len(l.prefix):], Machine: i.ID})
	}
	answer, err := l.cfg.Platform.Reconcile(ctx, observed)
	if err != nil {
		return fmt.Errorf("decide desired state: %w", err)
	}
	want, err := l.read(answer)
	if err != nil {
		return fmt.Errorf("desired state: %w", err)
	}

	p := &pass{l: l, ready: l.readyPool(pool), started: map[string]bool{}}
	for _, name := range sortedKeys(byName) {
		if _, wanted := want.instances[name]; !wanted && !want.ending[name] {
			for _, i := range byName[name] {
				p.destroy(ctx, i, "no longer desired")
			}
		}
	}
	p.create(ctx, want, byName)

	// An ending instance is told to stop on every pass until its command has exited: the first
	// signal starts the agent's last turn and the instance's supervisor ignores the rest. Once its
	// command has exited its last turn is over and it is destroyed, its end already reported with
	// this pass's question.
	for _, name := range sortedKeys(want.ending) {
		for _, i := range byName[name] {
			if i.State == provider.StateExited {
				p.destroy(ctx, i, "its last turn ended")
				continue
			}
			if err := l.provider.Stop(ctx, i.ID); err != nil {
				l.cfg.Log("stop %s (ending), instance %s: %v", i.Name, i.ID, err)
				p.failures = append(p.failures, fmt.Errorf("stop %s: %w", i.Name, err))
			}
		}
	}

	p.keepPool(ctx, pool, want)
	return errors.Join(p.failures...)
}

// confirmUnlisted is rented with every machine the platform holds that the list leaves out
// under its name read by its id: the provider's list can trail its instances, and the platform
// takes a machine it saw and is no longer told of for gone and runs its agent on another. Only
// a read that says the machine is gone leaves it out, and a read that fails fails the pass, so
// nothing is reported from a list that may be missing a machine. A read that finds the machine
// replaces whatever the list says of that id.
func (l *Launcher) confirmUnlisted(ctx context.Context, rented []provider.Instance, held []sandboxapi.HeldMachine) ([]provider.Instance, error) {
	for _, h := range held {
		name := l.prefix + h.Name
		listed := false
		for _, i := range rented {
			listed = listed || i.ID == h.Machine && i.Name == name
		}
		if listed {
			continue
		}
		found, exists, err := l.provider.Get(ctx, h.Machine)
		if err != nil {
			return nil, fmt.Errorf("instance %s of %s is not listed and could not be read: %w", h.Machine, name, err)
		}
		kept := []provider.Instance{}
		for _, i := range rented {
			if i.ID != h.Machine {
				kept = append(kept, i)
			}
		}
		renamed := len(kept) < len(rented)
		rented = kept
		if !exists {
			continue
		}
		if !strings.HasPrefix(found.Name, l.prefix) {
			return nil, fmt.Errorf("instance %s of %s is not listed, and read by its id it is named %q, outside this launcher's names", h.Machine, name, found.Name)
		}
		// A warm instance started for an agent is listed under the agent's name, which is no gap.
		if !renamed {
			l.cfg.Log("instance %s of %s is %s and the provider's list left it out", h.Machine, name, found.State)
		}
		rented = append(rented, found)
	}
	return rented, nil
}

// pass is what one Pass keeps while it acts on an answer.
type pass struct {
	l *Launcher
	// ready is the warm pool's instances that can be started, by pool name, oldest first.
	ready map[string][]provider.Instance
	// started is the pool instances this pass took, by id, which are no longer the pool's.
	started  map[string]bool
	rentals  int
	failures []error
}

func (p *pass) destroy(ctx context.Context, i provider.Instance, why string) bool {
	if err := p.l.provider.Destroy(ctx, i.ID); err != nil {
		p.l.cfg.Log("destroy %s (%s), instance %s: %v", i.Name, why, i.ID, err)
		p.failures = append(p.failures, fmt.Errorf("destroy %s: %w", i.Name, err))
		return false
	}
	p.l.cfg.Log("destroyed %s (%s), instance %s", i.Name, why, i.ID)
	return true
}

// rent waits the pause before every rental of the pass but the first, and reports false when
// ctx ended first.
func (p *pass) rent(ctx context.Context) bool {
	if p.rentals > 0 && !p.l.pause(ctx) {
		return false
	}
	p.rentals++
	return true
}

func (p *pass) create(ctx context.Context, want *desired, byName map[string][]provider.Instance) {
	l := p.l
	var creates, failedCreates int
	var lastCreateError error
	for _, name := range sortedKeys(want.instances) {
		// One instance runs the agent: the live one with the lowest id. A provider that lists
		// with a lag or a create that failed after it succeeded leaves a second, which goes.
		//
		// An exited one is never rented again from here, and one that stays starting is never
		// replaced from here. Both were reported with this pass's question, and the platform
		// answers one it gives up on with another name or with none. An exited one still
		// asked for under its own name is left as it is for the next answer, and goes only as
		// the leftover beside a live one.
		var live, exited []provider.Instance
		cleared := true
		instances := byName[name]
		sort.SliceStable(instances, func(a, b int) bool { return instances[a].ID < instances[b].ID })
		for _, i := range instances {
			switch {
			case i.State == provider.StateExited:
				exited = append(exited, i)
			case len(live) > 0:
				cleared = p.destroy(ctx, i, "duplicate") && cleared
			default:
				live = append(live, i)
			}
		}
		if len(live) > 0 {
			for _, i := range exited {
				p.destroy(ctx, i, "its command exited beside a live instance of the name")
			}
			continue
		}
		if len(exited) > 0 {
			l.cfg.Log("%s exited and is still asked for: reported, and left for the platform's next answer", name)
			continue
		}
		if !cleared {
			continue
		}
		spec := l.spec(want.instances[name])
		if warm, ok := p.warmFor(spec); ok {
			p.startWarm(ctx, warm, spec)
			continue
		}
		if !p.rent(ctx) {
			p.failures = append(p.failures, fmt.Errorf("create %s: %w", name, ctx.Err()))
			break
		}
		creates++
		id, err := l.provider.Create(ctx, spec)
		if err != nil {
			l.cfg.Log("create %s: %v", name, err)
			p.failures = append(p.failures, fmt.Errorf("create %s: %w", name, err))
			failedCreates++
			lastCreateError = err
			continue
		}
		l.cfg.Log("created %s (asked for and not rented), instance %s", name, id)
	}
	// Every create failing is rarely one agent's fault, so it is said once more in a line of its
	// own. Nothing is given up on here: the platform asks again until a machine has gone
	// unlisted for its boot limit, spends a restart then, and starts every agent afresh once the
	// image or the sandbox settings change.
	if creates > 0 && failedCreates == creates {
		l.cfg.Log("NO INSTANCE COULD BE CREATED: all %d creates of this pass failed, the last with: %v. Look at the provider account and its limits, the agents' image, and sandbox.instance in the settings", creates, lastCreateError)
	}
}

const (
	// exitLogLines bounds the end of an exited instance's output that is sent to the control
	// plane, besides sandboxapi.MaxExitLogBytes.
	exitLogLines = 60
	// progressLines is how far back in a running instance's output its newest progress marker
	// is looked for. An agent prints one every few seconds and little else that fast.
	progressLines = 200
)

// The lines an agent's harness prints while it works. What follows the word is only the control
// plane's to give a meaning to: progress is a marker, activity a sentence it shows of what the
// agent is doing, and state what the harness runs and the rate limit it last read.
var (
	progressPattern     = regexp.MustCompile(fmt.Sprintf(`^%s ([A-Za-z0-9._:-]{1,%d})$`, sandboxapi.ProgressPrefix, sandboxapi.MaxProgressMarker))
	activityPattern     = regexp.MustCompile(fmt.Sprintf(`^%s (.+)$`, sandboxapi.ActivityPrefix))
	harnessStatePattern = regexp.MustCompile(fmt.Sprintf(`^%s (.{1,%d})$`, sandboxapi.StatePrefix, sandboxapi.MaxStateLength))
)

// lastMatch is what pattern captures of the newest of lines it matches, or "" when none does.
func lastMatch(lines []string, pattern *regexp.Regexp) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if found := pattern.FindStringSubmatch(strings.TrimRight(lines[i], "\r")); found != nil {
			return found[1]
		}
	}
	return ""
}

// activityOf is what the control plane is told the agent whose output ends in lines is doing: its
// newest sentence, cut to what the control plane takes.
func activityOf(lines []string) string {
	said := []rune(lastMatch(lines, activityPattern))
	if len(said) > sandboxapi.MaxActivityLength {
		said = said[:sandboxapi.MaxActivityLength]
	}
	return string(said)
}

// liveness orders the states of the instances of one name by how much each says the agent runs.
// The name is reported in the highest.
var liveness = map[provider.State]int{
	provider.StateExited: 0, provider.StateStarting: 1, provider.StateUnknown: 2, provider.StateRunning: 3,
}

// reported is the state the control plane is told for each state a name is reported in.
var reported = map[provider.State]sandboxapi.InstanceState{
	provider.StateExited: sandboxapi.InstanceExited, provider.StateStarting: sandboxapi.InstanceStarting,
	provider.StateUnknown: sandboxapi.InstanceUnknown, provider.StateRunning: sandboxapi.InstanceRunning,
}

// observe is what the control plane is told with the question: every name of this launcher's
// namespace once, live if any instance of it is, for a name whose instances all exited the end
// of the output of the newest and whether its vendor's own lifetime ended it, and for a running one the progress marker its command last
// printed and what it last said its agent is doing, both from one read of its output. A marker
// that could not be read is left out, which the control plane tells apart from one that was
// never printed. The output is sent and never logged here.
func (l *Launcher) observe(ctx context.Context, byName map[string][]provider.Instance) []sandboxapi.ObservedInstance {
	observed := []sandboxapi.ObservedInstance{}
	for _, name := range sortedKeys(byName) {
		instances := byName[name]
		sort.SliceStable(instances, func(a, b int) bool { return instances[a].ID < instances[b].ID })
		state, last := provider.StateExited, instances[len(instances)-1]
		// The name is reported by the machine its state is taken from.
		reportedBy := last
		var running *provider.Instance
		for _, i := range instances {
			if liveness[i.State] > liveness[state] {
				state, reportedBy = i.State, i
			}
			if i.State == provider.StateRunning && running == nil {
				running = &i
			}
		}
		entry := sandboxapi.ObservedInstance{Name: name[len(l.prefix):], Machine: reportedBy.ID, State: reported[state]}
		if running != nil {
			if lines, ok := l.progressOf(ctx, *running); ok {
				progress := lastMatch(lines, progressPattern)
				entry.Progress = &progress
				entry.Activity = activityOf(lines)
				entry.HarnessState = lastMatch(lines, harnessStatePattern)
			}
		}
		if state == provider.StateExited {
			entry.Log = l.exitLogOf(ctx, last)
			entry.Expired = last.Expired
		}
		observed = append(observed, entry)
	}
	return observed
}

// progressOf is the end of a running instance's output, where its progress lines are.
func (l *Launcher) progressOf(ctx context.Context, i provider.Instance) ([]string, bool) {
	lines, err := l.provider.Logs(ctx, i.ID, progressLines)
	if err != nil {
		l.cfg.Log("read the progress of running %s: %v", i.Name, err)
		return nil, false
	}
	return lines, true
}

// exitLogOf is the end of an exited instance's output: an instance shows as exited only when its
// output is complete.
func (l *Launcher) exitLogOf(ctx context.Context, i provider.Instance) string {
	lines, err := l.provider.Logs(ctx, i.ID, exitLogLines)
	if err != nil {
		l.cfg.Log("read the output of exited %s: %v", i.Name, err)
		return ""
	}
	return tail(strings.Join(lines, "\n"), sandboxapi.MaxExitLogBytes)
}

func tail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[len(text)-limit:], "")
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
	env := make(map[string]string, len(i.env))
	for name, value := range i.env {
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
