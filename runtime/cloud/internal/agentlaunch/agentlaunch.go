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
	// CreatePause is waited between two creates of one pass, for a provider that limits how fast
	// instances may be rented. Zero is no pause.
	CreatePause time.Duration
	Log         func(format string, args ...any)
}

type Launcher struct {
	cfg          Config
	prefix       string
	provider     provider.Provider
	launchDigest string
	maxAnswer    int
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
	return &Launcher{
		cfg: cfg, prefix: provider.AgentPrefix(cfg.Name), provider: provider.Bounded(cfg.Provider),
		launchDigest: digest, maxAnswer: maxAnswerBytes,
	}, nil
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
	if cfg.CreatePause < 0 {
		return errors.New("agentlaunch: CreatePause must not be negative")
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
func (l *Launcher) Run(ctx context.Context) error {
	ticker := time.NewTicker(l.cfg.Interval)
	defer ticker.Stop()
	for {
		if err := l.Pass(ctx); err != nil {
			l.cfg.Log("pass: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Pass is one convergence. The provider is listed and the control plane told what was found
// before anything is touched, so a provider that cannot be listed, or a control plane that cannot
// be reached or answers with anything but a complete list, changes nothing. After that every
// instance is handled by itself: one that cannot be created, stopped or destroyed is reported in
// the returned error and the rest of the pass still runs.
//
// An answer that states a refusal is acted on like any other, except that nothing is created,
// and the pass returns ErrRefused with the control plane's words.
func (l *Launcher) Pass(ctx context.Context) error {
	rented, err := l.provider.List(ctx, l.prefix)
	// The launcher pauses nothing yet (see the provider package's comment on capabilities).
	rented = provider.PausedAsStarting(rented)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	byName := map[string][]provider.Instance{}
	for _, i := range rented {
		byName[i.Name] = append(byName[i.Name], i)
	}
	want, err := l.fetchDesired(ctx, l.observe(ctx, byName))
	if err != nil {
		return fmt.Errorf("fetch desired state: %w", err)
	}

	var failures []error
	destroy := func(i provider.Instance, why string) bool {
		if err := l.provider.Destroy(ctx, i.ID); err != nil {
			l.cfg.Log("destroy %s (%s), instance %s: %v", i.Name, why, i.ID, err)
			failures = append(failures, fmt.Errorf("destroy %s: %w", i.Name, err))
			return false
		}
		l.cfg.Log("destroyed %s (%s), instance %s", i.Name, why, i.ID)
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

	// An ending instance is told to stop on every pass until its command has exited: the signal
	// is what starts the agent's last turn, and the provider takes it any number of times.
	for _, name := range sortedKeys(want.ending) {
		for _, i := range byName[name] {
			if i.State == provider.StateExited {
				continue
			}
			if err := l.provider.Stop(ctx, i.ID); err != nil {
				l.cfg.Log("stop %s (ending), instance %s: %v", i.Name, i.ID, err)
				failures = append(failures, fmt.Errorf("stop %s: %w", i.Name, err))
				continue
			}
			l.cfg.Log("told %s to stop (ending), instance %s", i.Name, i.ID)
		}
	}

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
				cleared = destroy(i, "duplicate") && cleared
			default:
				live = append(live, i)
			}
		}
		if len(live) > 0 {
			for _, i := range exited {
				destroy(i, "its command exited beside a live instance of the name")
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
		// The instance ends by itself at the moment the control plane names, whether or not
		// this launcher can still reach it then.
		lifetime := time.Until(want.instances[name].endsAt).Truncate(time.Second)
		if lifetime <= 0 {
			err := fmt.Errorf("create %s: its ends_at %s has passed", name, want.instances[name].endsAt.Format(time.RFC3339))
			l.cfg.Log("%v", err)
			failures = append(failures, err)
			continue
		}
		if creates > 0 && !l.pause(ctx) {
			failures = append(failures, fmt.Errorf("create %s: %w", name, ctx.Err()))
			break
		}
		creates++
		spec := l.spec(want.instances[name], lifetime)
		id, err := l.provider.Create(ctx, spec)
		if err != nil {
			err = withoutEnvironment(err, spec.Env)
			l.cfg.Log("create %s: %v", name, err)
			failures = append(failures, fmt.Errorf("create %s: %w", name, err))
			failedCreates++
			lastCreateError = err
			continue
		}
		l.cfg.Log("created %s (asked for and not rented), instance %s, which ends itself at %s", name, id, want.instances[name].endsAt.UTC().Format(time.RFC3339))
	}
	if want.refusal != "" {
		failures = append(failures, fmt.Errorf("%w, so nothing is created: %s", ErrRefused, want.refusal))
	}
	// Every create failing is rarely one agent's fault, so it is said once more in a line of its
	// own. Nothing is given up on here: the control plane asks again until a machine has gone
	// unlisted for its boot limit, spends a restart then, and starts every agent afresh once the
	// image or this launcher's config changes.
	if creates > 0 && failedCreates == creates {
		l.cfg.Log("NO INSTANCE COULD BE CREATED: all %d creates of this pass failed, the last with: %v. Look at the provider account and its limits, the image the control plane names, and instance in this launcher's config", creates, lastCreateError)
	}
	return errors.Join(failures...)
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

// observe is what the control plane is told with the question: every name of this launcher's
// namespace once, live if any instance of it is, for a name whose instances all exited the end
// of the output of the newest, and for a running one the progress marker its command last
// printed. A marker that could not be read is left out, which the control plane tells apart from
// one that was never printed. The output is sent and never logged here.
func (l *Launcher) observe(ctx context.Context, byName map[string][]provider.Instance) []observedInstance {
	observed := []observedInstance{}
	for _, name := range sortedKeys(byName) {
		instances := byName[name]
		sort.SliceStable(instances, func(a, b int) bool { return instances[a].ID < instances[b].ID })
		state, last := provider.StateExited, instances[len(instances)-1]
		var running *provider.Instance
		for _, i := range instances {
			if i.State == provider.StateRunning || (i.State == provider.StateStarting && state != provider.StateRunning) {
				state = i.State
			}
			if i.State == provider.StateRunning && running == nil {
				running = &i
			}
		}
		entry := observedInstance{Name: name[len(l.prefix):], State: string(state)}
		if running != nil {
			if lines, err := l.provider.Logs(ctx, running.ID, progressLines); err != nil {
				l.cfg.Log("read the progress of running %s: %v", name, err)
			} else {
				progress := lastProgress(lines)
				entry.Progress = &progress
			}
		}
		if state == provider.StateExited {
			lines, err := l.provider.Logs(ctx, last.ID, exitLogLines)
			if err != nil {
				l.cfg.Log("read the output of exited %s: %v", name, err)
			}
			entry.Log = tail(l.withoutOwnEnvironment(strings.Join(lines, "\n")), exitLogBytes)
		}
		observed = append(observed, entry)
	}
	return observed
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

func (l *Launcher) spec(i instance, lifetime time.Duration) provider.Spec {
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
		Lifetime:    lifetime,
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
