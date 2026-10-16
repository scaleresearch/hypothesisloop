package agentlaunch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/launcherapi"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// Size is what every agent instance is rented at.
type Size struct {
	CPUMillis   int64
	MemoryBytes int64
	DiskBytes   int64
}

type Config struct {
	// Name is the launcher the control plane knows this process as. It is part of every instance
	// name: the launcher lists, creates and destroys the instances that start with it and no
	// others, so two launchers on one provider account keep apart by their names.
	Name   string
	APIURL string
	// Token is this launcher's own token, sent on every call to the control plane.
	Token string
	// Command is the argv every instance runs.
	Command []string
	Size    Size
	// ModelProvider names whose models the agents run on, and Env is what that provider's
	// harness needs in every instance: its credential. Env is added here, so what it holds never
	// passes through the control plane, and the control plane's list may not set any of its names.
	ModelProvider string
	Env           map[string]string
	Provider      provider.Provider
	HTTPClient    *http.Client
	Interval      time.Duration
	// CreatePause is waited between two rentals of one pass (a create or a warm), for a provider
	// that limits how fast instances may be rented. Zero is no pause. Starting a warm instance is
	// no rental and is never paused for.
	CreatePause time.Duration
	// ProgressInterval is how often a running instance's output is read for its progress when the
	// passes come faster: a pass in between reports what the last read found. Zero reads it on
	// every pass.
	ProgressInterval time.Duration
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
	maxAnswer    int
	// outputs is what was last read of each instance's output, by instance id, to spare the
	// provider's log API a read on every pass: a running instance's progress for
	// ProgressInterval, and an exited instance's end for as long as it is listed. Losing it
	// costs a read and nothing else.
	outputs map[string]outputRead
	// stopped is when each ending instance was last told to stop, by instance id.
	stopped map[string]time.Time
	now     func() time.Time
}

// stopRetry is how long an ending instance that is still running waits before it is told to stop
// again: one stop starts the agent's last turn, and a launcher once sent one to the same machine
// every pass for a minute.
const stopRetry = 2 * time.Minute

type outputRead struct {
	at time.Time
	// exited says that log is the end of the output of an instance that exited.
	exited                           bool
	progress, activity, harnessState string
	log                              string
}

func New(cfg Config) (*Launcher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Token == "" || cfg.Provider == nil || cfg.HTTPClient == nil || cfg.Log == nil {
		return nil, errors.New("agentlaunch: Token, Provider, HTTPClient and Log are required")
	}
	digest, err := cfg.launchDigest()
	if err != nil {
		return nil, err
	}
	l := &Launcher{
		cfg: cfg, prefix: provider.AgentPrefix(cfg.Name), provider: provider.Bounded(cfg.Provider),
		launchDigest: digest, maxAnswer: maxAnswerBytes, outputs: map[string]outputRead{},
		stopped: map[string]time.Time{}, now: time.Now,
	}
	l.poolPrefix = l.prefix + poolSegment
	l.warmer, _ = l.provider.(provider.Warmer)
	if cfg.WarmPool > 0 && l.warmer == nil {
		return nil, fmt.Errorf("agentlaunch: a warm pool of %d needs a provider that can warm instances, and this one can %v", cfg.WarmPool, provider.Capabilities(cfg.Provider))
	}
	return l, nil
}

// launchDigest is a digest of everything this launcher itself starts an instance with. The
// control plane makes it part of each instance's name, so a changed command, size or credential
// replaces the running instances as a changed image does. It holds a credential only hashed
// together with the rest.
func (cfg Config) launchDigest() (string, error) {
	inputs, err := json.Marshal(struct {
		Command       []string          `json:"command"`
		Size          Size              `json:"size"`
		ModelProvider string            `json:"model_provider"`
		Env           map[string]string `json:"env"`
	}{cfg.Command, cfg.Size, cfg.ModelProvider, cfg.Env})
	if err != nil {
		return "", fmt.Errorf("agentlaunch: digest launch inputs: %w", err)
	}
	digest := sha256.Sum256(inputs)
	return hex.EncodeToString(digest[:]), nil
}

// Validate checks what a config file decides, so the file is refused when it is loaded, before
// the provider is built or the control plane contacted.
func (cfg Config) Validate() error {
	if cfg.APIURL == "" {
		return errors.New("agentlaunch: APIURL is required")
	}
	if err := provider.ValidateSegment("launcher name", cfg.Name); err != nil {
		return fmt.Errorf("agentlaunch: %w", err)
	}
	if cfg.Interval%time.Second != 0 || cfg.CreatePause%time.Millisecond != 0 {
		return errors.New("agentlaunch: Interval is whole seconds and CreatePause whole milliseconds, as the control plane is told them")
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
	if cfg.CreatePause < 0 || cfg.ProgressInterval < 0 || cfg.WarmPool < 0 {
		return errors.New("agentlaunch: CreatePause, ProgressInterval and WarmPool must not be negative")
	}
	if cfg.ModelProvider == "" || len(cfg.Env) == 0 {
		return errors.New("agentlaunch: ModelProvider and its Env are required")
	}
	for name, value := range cfg.Env {
		if name == "" || value == "" {
			return fmt.Errorf("agentlaunch: Env entry %q needs a name and a value", name)
		}
	}
	return nil
}

// Run passes until ctx is cancelled. A pass that fails is logged and the next one starts from
// nothing, like every other. The control plane refusing this launcher's configuration ends
// nothing either, on the first pass or a later one: this process is the only thing that stops and
// destroys the instances under its name, an earlier process's among them, so it goes on passing
// and says the refusal on every pass until the config or the platform's limits are corrected.
//
// Once ctx is cancelled the warm pool is destroyed, and the agents' instances are left to the
// next process.
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

// Pass is one convergence. The provider is listed and the control plane told what was found
// before anything is touched, so a provider that cannot be listed, or a control plane that cannot
// be reached or answers with anything but a complete list, changes nothing. A machine the
// platform holds that the list leaves out is read by its id first (confirmUnlisted). After that every
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
	held, err := l.fetchHeld(ctx)
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
	l.forgetUnlisted(rented)
	want, err := l.fetchDesired(ctx, l.observe(ctx, byName))
	if err != nil {
		return fmt.Errorf("fetch desired state: %w", err)
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

	// An ending instance is told to stop once, and again every stopRetry until its command has
	// exited: the signal is what starts the agent's last turn. A stop that failed is sent again
	// on the next pass. Once its command has exited its last turn is over and it is destroyed,
	// its end already reported with this pass's question: a coordinator was once kept four
	// minutes after it, to the end of the control plane's wind-down.
	for _, name := range sortedKeys(want.ending) {
		for _, i := range byName[name] {
			if i.State == provider.StateExited {
				p.destroy(ctx, i, "its last turn ended")
				continue
			}
			if at, told := l.stopped[i.ID]; told && l.now().Sub(at) < stopRetry {
				continue
			}
			if err := l.provider.Stop(ctx, i.ID); err != nil {
				l.cfg.Log("stop %s (ending), instance %s: %v", i.Name, i.ID, err)
				p.failures = append(p.failures, fmt.Errorf("stop %s: %w", i.Name, err))
				continue
			}
			l.stopped[i.ID] = l.now()
			l.cfg.Log("told %s to stop (ending), instance %s", i.Name, i.ID)
		}
	}

	p.keepPool(ctx, pool, want)
	if want.refusal != "" {
		p.failures = append(p.failures, fmt.Errorf("%w, so nothing is created: %s", ErrRefused, want.refusal))
	}
	return errors.Join(p.failures...)
}

// confirmUnlisted is rented with every machine the platform holds that the list leaves out
// under its name read by its id: the provider's list can trail its instances, and the platform
// takes a machine it saw and is no longer told of for gone and runs its agent on another. Only
// a read that says the machine is gone leaves it out, and a read that fails fails the pass, so
// nothing is reported from a list that may be missing a machine. A read that finds the machine
// replaces whatever the list says of that id.
func (l *Launcher) confirmUnlisted(ctx context.Context, rented []provider.Instance, held []launcherapi.HeldMachine) ([]provider.Instance, error) {
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
		rented = kept
		if !exists {
			continue
		}
		if !strings.HasPrefix(found.Name, l.prefix) {
			return nil, fmt.Errorf("instance %s of %s is not listed, and read by its id it is named %q, outside this launcher's names", h.Machine, name, found.Name)
		}
		l.cfg.Log("instance %s of %s is %s and the provider's list left it out", h.Machine, name, found.State)
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
		// replaced from here. Both were reported with this pass's question, and the control
		// plane answers one it gives up on with another name or with none. An exited one still
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
			l.cfg.Log("%s exited and is still asked for: reported, and left for the control plane's next answer", name)
			continue
		}
		if !cleared || want.refusal != "" {
			continue
		}
		spec := l.spec(want.instances[name])
		if id, ok := p.startWarm(ctx, spec); ok {
			l.cfg.Log("started %s (asked for and not rented) on warm instance %s", name, id)
			continue
		}
		if !p.rent(ctx) {
			p.failures = append(p.failures, fmt.Errorf("create %s: %w", name, ctx.Err()))
			break
		}
		creates++
		id, err := l.provider.Create(ctx, spec)
		if err != nil {
			err = withoutEnvironment(err, spec.Env)
			l.cfg.Log("create %s: %v", name, err)
			p.failures = append(p.failures, fmt.Errorf("create %s: %w", name, err))
			failedCreates++
			lastCreateError = err
			continue
		}
		l.cfg.Log("created %s (asked for and not rented), instance %s", name, id)
	}
	// Every create failing is rarely one agent's fault, so it is said once more in a line of its
	// own. Nothing is given up on here: the control plane asks again until a machine has gone
	// unlisted for its boot limit, spends a restart then, and starts every agent afresh once the
	// image or this launcher's config changes.
	if creates > 0 && failedCreates == creates {
		l.cfg.Log("NO INSTANCE COULD BE CREATED: all %d creates of this pass failed, the last with: %v. Look at the provider account and its limits, the image the control plane names, and instance in this launcher's config", creates, lastCreateError)
	}
}

const (
	// exitLogLines and exitLogBytes bound the end of an exited instance's output that is sent
	// to the control plane.
	exitLogLines = 60
	exitLogBytes = 4 << 10
	// progressLines is how far back in a running instance's output its newest progress marker
	// is looked for. An agent prints one every few seconds and little else that fast.
	progressLines = 200
)

// progressPattern is the line an agent's harness prints while it works or waits as it should.
// What follows the word is the marker, which only the control plane gives a meaning to.
var progressPattern = regexp.MustCompile(`^hl-progress ([A-Za-z0-9._:-]{1,64})$`)

// lastProgress is the marker of the newest progress line among lines, or "" when none is one.
func lastProgress(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if found := progressPattern.FindStringSubmatch(strings.TrimRight(lines[i], "\r")); found != nil {
			return found[1]
		}
	}
	return ""
}

// activityPattern is the line an agent's harness prints beside its progress to say what the
// agent is doing: a sentence the control plane stores and shows, and gives no other meaning to.
var activityPattern = regexp.MustCompile(`^hl-activity (.+)$`)

// maxActivityLength is the longest sentence the control plane takes, in characters.
const maxActivityLength = 120

// lastActivity is the sentence of the newest activity line among lines, or "" when none is one.
func lastActivity(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if found := activityPattern.FindStringSubmatch(strings.TrimRight(lines[i], "\r")); found != nil {
			return found[1]
		}
	}
	return ""
}

// harnessStatePattern is the line an agent's harness prints with its progress to say what it is
// running and the model provider's rate limit it last read, which only the control plane gives a
// meaning to.
var harnessStatePattern = regexp.MustCompile(`^hl-state (.{1,200})$`)

// lastHarnessState is what follows the word on the newest state line among lines, or "".
func lastHarnessState(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if found := harnessStatePattern.FindStringSubmatch(strings.TrimRight(lines[i], "\r")); found != nil {
			return found[1]
		}
	}
	return ""
}

// activityOf is what the control plane is told the agent whose output ends in lines is doing: its
// newest sentence without this launcher's own environment, which may make it longer, cut to what
// the control plane takes.
func (l *Launcher) activityOf(lines []string) string {
	said := []rune(l.withoutOwnEnvironment(lastActivity(lines)))
	if len(said) > maxActivityLength {
		said = said[:maxActivityLength]
	}
	return string(said)
}

// liveness orders the states of the instances of one name by how much each says the agent runs.
// The name is reported in the highest.
var liveness = map[provider.State]int{
	provider.StateExited: 0, provider.StateStarting: 1, provider.StateUnknown: 2, provider.StateRunning: 3,
}

// observe is what the control plane is told with the question: every name of this launcher's
// namespace once, live if any instance of it is, for a name whose instances all exited the end
// of the output of the newest and whether its vendor's own lifetime ended it, and for a running one the progress marker its command last
// printed and what it last said its agent is doing, both from one read of its output. A marker
// that could not be read is left out, which the control plane tells apart from one that was
// never printed. The output is sent and never logged here.
func (l *Launcher) observe(ctx context.Context, byName map[string][]provider.Instance) []launcherapi.ObservedInstance {
	observed := []launcherapi.ObservedInstance{}
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
		entry := launcherapi.ObservedInstance{Name: name[len(l.prefix):], Machine: reportedBy.ID, State: string(state)}
		if running != nil {
			if seen, ok := l.progressOf(ctx, *running); ok {
				entry.Progress = &seen.progress
				entry.Activity = seen.activity
				entry.HarnessState = seen.harnessState
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

// progressOf is the progress and activity of a running instance, read from its output unless a
// read of it is younger than ProgressInterval and the passes come faster than that.
func (l *Launcher) progressOf(ctx context.Context, i provider.Instance) (outputRead, bool) {
	if seen, ok := l.outputs[i.ID]; ok && !seen.exited && l.cfg.Interval < l.cfg.ProgressInterval && time.Since(seen.at) < l.cfg.ProgressInterval {
		return seen, true
	}
	lines, err := l.provider.Logs(ctx, i.ID, progressLines)
	if err != nil {
		l.cfg.Log("read the progress of running %s: %v", i.Name, err)
		return outputRead{}, false
	}
	seen := outputRead{at: time.Now(), progress: lastProgress(lines), activity: l.activityOf(lines), harnessState: lastHarnessState(lines)}
	l.outputs[i.ID] = seen
	return seen, true
}

// exitLogOf is the end of an exited instance's output, read once: an instance shows as exited
// only when its output is complete.
func (l *Launcher) exitLogOf(ctx context.Context, i provider.Instance) string {
	if seen, ok := l.outputs[i.ID]; ok && seen.exited {
		return seen.log
	}
	lines, err := l.provider.Logs(ctx, i.ID, exitLogLines)
	if err != nil {
		l.cfg.Log("read the output of exited %s: %v", i.Name, err)
		return ""
	}
	log := tail(l.withoutOwnEnvironment(strings.Join(lines, "\n")), exitLogBytes)
	l.outputs[i.ID] = outputRead{at: time.Now(), exited: true, log: log}
	return log
}

// forgetUnlisted drops what is kept of instances that are no longer listed: what was read of
// them and when they were told to stop.
func (l *Launcher) forgetUnlisted(listed []provider.Instance) {
	ids := make(map[string]bool, len(listed))
	for _, i := range listed {
		ids[i.ID] = true
	}
	for id := range l.outputs {
		if !ids[id] {
			delete(l.outputs, id)
		}
	}
	for id := range l.stopped {
		if !ids[id] {
			delete(l.stopped, id)
		}
	}
}

// withoutOwnEnvironment is text with every value this launcher adds to an instance's environment
// taken out: an agent may print the model provider's credential, and its output is stored by the
// control plane. The instance's own token is the control plane's to take out, which minted it.
func (l *Launcher) withoutOwnEnvironment(text string) string {
	for _, name := range sortedKeys(l.cfg.Env) {
		if value := l.cfg.Env[name]; len(value) >= minRedactedLength {
			text = strings.ReplaceAll(text, value, "<"+name+">")
		}
	}
	return text
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

// minRedactedLength keeps short values such as a flavor readable in an error: nothing that
// short is a secret.
const minRedactedLength = 8

// withoutEnvironment is err with every value of the instance's environment taken out of its
// text. A vendor that refuses a create may quote the request back, and the request holds the
// agent's token and the model provider's credential.
func withoutEnvironment(err error, env map[string]string) error {
	text := err.Error()
	for _, name := range sortedKeys(env) {
		if value := env[name]; len(value) >= minRedactedLength {
			text = strings.ReplaceAll(text, value, "<"+name+">")
		}
	}
	if text == err.Error() {
		return err
	}
	return errors.New(text)
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
