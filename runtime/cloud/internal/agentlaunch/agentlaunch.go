package agentlaunch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	// notCreated is what the last pass's creates ended with and the next question carries. It is
	// that pass's outcome and nothing is decided from it here: a launcher that restarts sends
	// none, fails the same creates again and reports them one pass later.
	notCreated []createFailure
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
		cfg: cfg, prefix: provider.AgentPrefix(cfg.Namespace), provider: provider.Bounded(cfg.Provider),
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

// Pass is one convergence. The provider is listed and the control plane told what was found
// before anything is touched, so a provider that cannot be listed, or a control plane that cannot
// be reached or answers with anything but a complete list, changes nothing. After that every
// instance is handled by itself: one that cannot be created, stopped or destroyed is reported in
// the returned error and the rest of the pass still runs.
func (l *Launcher) Pass(ctx context.Context) error {
	rented, err := l.provider.List(ctx, l.prefix)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	byName := map[string][]provider.Instance{}
	for _, i := range rented {
		byName[i.Name] = append(byName[i.Name], i)
	}
	// A name the provider lists by now was created after all, and is reported as what it is.
	var notCreated []createFailure
	for _, failure := range l.notCreated {
		if _, listed := byName[l.prefix+failure.Name]; !listed {
			notCreated = append(notCreated, failure)
		}
	}
	want, err := l.fetchDesired(ctx, l.observe(ctx, byName), notCreated)
	if err != nil {
		return fmt.Errorf("fetch desired state: %w", err)
	}

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

	// An ending instance is told to stop on every pass until its command has exited: the signal
	// is what starts the agent's last turn, and the provider takes it any number of times.
	for _, name := range sortedKeys(want.ending) {
		for _, i := range byName[name] {
			if i.State == provider.StateExited {
				continue
			}
			if err := l.provider.Stop(ctx, i.ID); err != nil {
				l.cfg.Log("stop %s (ending): %v", i.Name, err)
				failures = append(failures, fmt.Errorf("stop %s: %w", i.Name, err))
				continue
			}
			l.cfg.Log("told %s to stop (ending)", i.Name)
		}
	}

	var creates, failedCreates int
	var lastCreateError error
	l.notCreated = nil
	for _, name := range sortedKeys(want.instances) {
		// One instance runs the agent: the live one with the lowest id. A provider that lists
		// with a lag or a create that failed after it succeeded leaves a second, which goes.
		//
		// An exited one is never rented again from here, and one that stays starting is never
		// replaced from here. Both were reported with this pass's question, and a failed
		// create is reported with the next, and the control
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
		if !cleared {
			continue
		}
		if creates > 0 && !l.pause(ctx) {
			failures = append(failures, fmt.Errorf("create %s: %w", name, ctx.Err()))
			break
		}
		creates++
		spec := l.spec(want.instances[name])
		if _, err := l.provider.Create(ctx, spec); err != nil {
			err = withoutEnvironment(err, spec.Env)
			l.cfg.Log("create %s: %v", name, err)
			failures = append(failures, fmt.Errorf("create %s: %w", name, err))
			failedCreates++
			lastCreateError = err
			l.notCreated = append(l.notCreated, createFailure{Name: name[len(l.prefix):], Reason: tail(err.Error(), exitLogBytes)})
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

const (
	// exitLogLines and exitLogBytes bound the end of an exited instance's output that is sent
	// to the control plane.
	exitLogLines = 60
	exitLogBytes = 4 << 10
)

// observe is what the control plane is told with the question: every name of this launcher's
// namespace once, live if any instance of it is, and for a name whose instances all exited the
// end of the output of the newest. The output is sent and never logged here.
func (l *Launcher) observe(ctx context.Context, byName map[string][]provider.Instance) []observedInstance {
	observed := []observedInstance{}
	for _, name := range sortedKeys(byName) {
		instances := byName[name]
		sort.SliceStable(instances, func(a, b int) bool { return instances[a].ID < instances[b].ID })
		state, last := provider.StateExited, instances[len(instances)-1]
		for _, i := range instances {
			if i.State == provider.StateRunning || (i.State == provider.StateStarting && state != provider.StateRunning) {
				state = i.State
			}
		}
		entry := observedInstance{Name: name[len(l.prefix):], State: string(state)}
		if state == provider.StateExited {
			lines, err := l.provider.Logs(ctx, last.ID, exitLogLines)
			if err != nil {
				l.cfg.Log("read the output of exited %s: %v", name, err)
			}
			entry.Log = tail(strings.Join(lines, "\n"), exitLogBytes)
		}
		observed = append(observed, entry)
	}
	return observed
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
