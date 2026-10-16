package agentlaunch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/launcherapi"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/cpclient"
)

// instance is one entry of the control plane's list under the name the provider will hold it by.
type instance struct {
	name  string
	image string
	env   map[string]string
}

// desired is one answer of the control plane, keyed by full instance name: what should run, and
// what has just stopped being wanted and is left to end by itself. refusal is the control
// plane's words while it refuses this launcher's configuration: the lists are acted on and
// nothing is created.
type desired struct {
	instances map[string]instance
	ending    map[string]bool
	refusal   string
	// image is the platform's own image, "" from a control plane that does not say.
	image string
}

// maxAnswerBytes bounds what one answer may hold in memory.
const maxAnswerBytes = 64 << 20

// ErrRefused is the control plane refusing this launcher's own configuration. A pass returns it
// after it stopped and destroyed by the answer's lists, having created nothing.
var ErrRefused = errors.New("the control plane refuses this launcher's configuration")

// controlPlane is this launcher's API on the control plane.
func (l *Launcher) controlPlane() *cpclient.Client {
	client := &cpclient.Client{
		BaseURL: fmt.Sprintf("%s/internal/launchers/%s", l.cfg.APIURL, url.PathEscape(l.cfg.Name)),
		Token:   l.cfg.Token, HTTP: l.cfg.HTTPClient, MaxAnswerBytes: l.maxAnswer,
	}
	if l.cfg.Version != "" {
		client.Header = http.Header{clusterapi.AgentVersionHeader: {l.cfg.Version}}
	}
	return client
}

// fetchHeld asks the control plane which machines it holds for this launcher, which changes
// nothing there.
func (l *Launcher) fetchHeld(ctx context.Context) ([]launcherapi.HeldMachine, error) {
	var answer launcherapi.Held
	if err := l.controlPlane().Do(ctx, http.MethodGet, "/machines", nil, &answer); err != nil {
		return nil, err
	}
	if answer.Machines == nil {
		return nil, errors.New("answer carries no machines list")
	}
	for _, held := range answer.Machines {
		if held.Name == "" || held.Machine == "" {
			return nil, fmt.Errorf("a held machine has no name or no id: %+v", held)
		}
	}
	return answer.Machines, nil
}

func (l *Launcher) fetchDesired(ctx context.Context, observed []launcherapi.ObservedInstance) (*desired, error) {
	question := launcherapi.Observed{
		LaunchDigest: l.launchDigest, Instances: observed,
		ReconcileIntervalSeconds: int(l.cfg.Interval / time.Second), CreatePauseMilliseconds: int(l.cfg.CreatePause / time.Millisecond),
	}
	var answer launcherapi.Desired
	if err := l.controlPlane().Do(ctx, http.MethodPost, "/reconcile", question, &answer); err != nil {
		return nil, err
	}
	return l.read(answer)
}

// read turns an answer into the pass's view of it, or refuses the whole answer. Nothing in it is
// repaired or skipped: a list this launcher understands in part would have it destroy the agents
// it did not understand. A list that is absent or null decodes as nil, which is no empty list.
func (l *Launcher) read(answer launcherapi.Desired) (*desired, error) {
	if answer.Instances == nil || answer.Ending == nil {
		return nil, errors.New("answer carries no instances or no ending list")
	}
	want := &desired{instances: map[string]instance{}, ending: map[string]bool{}, image: answer.Image}
	for _, entry := range answer.Instances {
		name := l.prefix + entry.Name
		if entry.Name == "" || entry.Image == "" {
			return nil, fmt.Errorf("desired instance %q has no name or no image", entry.Name)
		}
		if err := provider.ValidateName(name); err != nil {
			return nil, fmt.Errorf("desired state: %w", err)
		}
		if strings.HasPrefix(entry.Name, poolSegment) {
			return nil, fmt.Errorf("desired instance %q starts with %q, which names this launcher's warm pool", entry.Name, poolSegment)
		}
		if _, twice := want.instances[name]; twice {
			return nil, fmt.Errorf("desired state names instance %q twice", entry.Name)
		}
		want.instances[name] = instance{name: name, image: entry.Image, env: entry.Env}
	}
	if answer.Refusal != nil {
		if answer.Refusal.Reason == "" || answer.Refusal.Message == "" {
			return nil, errors.New("answer carries a refusal with no reason or no message")
		}
		want.refusal = answer.Refusal.Reason + ": " + answer.Refusal.Message
	}
	for _, short := range answer.Ending {
		name := l.prefix + short
		if short == "" {
			return nil, errors.New("desired state names an ending instance with no name")
		}
		if strings.HasPrefix(short, poolSegment) {
			return nil, fmt.Errorf("ending instance %q starts with %q, which names this launcher's warm pool", short, poolSegment)
		}
		if _, wanted := want.instances[name]; wanted {
			return nil, fmt.Errorf("desired state names instance %q as both running and ending", short)
		}
		want.ending[name] = true
	}
	return want, nil
}
