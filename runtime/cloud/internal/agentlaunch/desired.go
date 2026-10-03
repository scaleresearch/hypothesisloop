package agentlaunch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// instance is one entry of the control plane's list under the name the provider will hold it by.
type instance struct {
	name  string
	image string
	env   map[string]string
	// endsAt is the moment after which the instance must not run. It is rented with the time
	// left until then as its lifetime.
	endsAt time.Time
}

// desired is one answer of the control plane, keyed by full instance name: what should run, and
// what has just stopped being wanted and is left to end by itself. refusal is the control
// plane's words while it refuses this launcher's configuration: the lists are acted on and
// nothing is created.
type desired struct {
	instances map[string]instance
	ending    map[string]bool
	refusal   string
}

// reconcileAnswer is the wire shape. Both lists are pointers because an answer without one is
// not an empty list: it is an answer this launcher cannot read, and acting on it would destroy
// every agent.
type reconcileAnswer struct {
	Instances *[]struct {
		Name   string            `json:"name"`
		Image  string            `json:"image"`
		Env    map[string]string `json:"env"`
		EndsAt time.Time         `json:"ends_at"`
	} `json:"instances"`
	Ending  *[]string `json:"ending"`
	Refusal *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"refusal"`
}

// maxAnswerBytes bounds what one answer may hold in memory.
const maxAnswerBytes = 64 << 20

// observedInstance is one name of this launcher's namespace as its provider lists it, without
// the launcher's prefix.
type observedInstance struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Log   string `json:"log,omitempty"`
	// Expired is provider.Instance.Expired of an exited instance.
	Expired bool `json:"expired,omitempty"`
	// Progress is nil when the output of a running instance could not be read.
	Progress *string `json:"progress,omitempty"`
	// Activity is what the agent of a running instance last said it is doing, empty when it
	// said nothing in the output the progress was read from.
	Activity string `json:"activity,omitempty"`
}

// reconcileQuestion is what the launcher tells the control plane with every question.
type reconcileQuestion struct {
	LaunchDigest string             `json:"launch_digest"`
	Instances    []observedInstance `json:"instances"`
	// How this launcher paces its passes, which the control plane holds against its boot limit.
	ReconcileIntervalSeconds int `json:"reconcile_interval_seconds"`
	CreatePauseMilliseconds  int `json:"create_pause_milliseconds"`
}

// ErrRefused is the control plane refusing this launcher's own configuration. A pass returns it
// after it stopped and destroyed by the answer's lists, having created nothing.
var ErrRefused = errors.New("the control plane refuses this launcher's configuration")

func (l *Launcher) fetchDesired(ctx context.Context, observed []observedInstance) (*desired, error) {
	question, err := json.Marshal(reconcileQuestion{
		LaunchDigest: l.launchDigest, Instances: observed,
		ReconcileIntervalSeconds: int(l.cfg.Interval / time.Second), CreatePauseMilliseconds: int(l.cfg.CreatePause / time.Millisecond),
	})
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/internal/launchers/%s/reconcile", l.cfg.APIURL, url.PathEscape(l.cfg.Name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(question))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+l.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(l.maxAnswer)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > l.maxAnswer {
		return nil, fmt.Errorf("answer is larger than %d bytes", l.maxAnswer)
	}
	var answer reconcileAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("decode answer: %w", err)
	}
	return l.read(answer)
}

// read turns an answer into the pass's view of it, or refuses the whole answer. Nothing in it is
// repaired or skipped: a list this launcher understands in part would have it destroy the agents
// it did not understand.
func (l *Launcher) read(answer reconcileAnswer) (*desired, error) {
	if answer.Instances == nil || answer.Ending == nil {
		return nil, errors.New("answer carries no instances or no ending list")
	}
	want := &desired{instances: map[string]instance{}, ending: map[string]bool{}}
	for _, entry := range *answer.Instances {
		name := l.prefix + entry.Name
		if entry.Name == "" || entry.Image == "" || entry.EndsAt.IsZero() {
			return nil, fmt.Errorf("desired instance %q has no name, no image or no ends_at", entry.Name)
		}
		if err := provider.ValidateName(name); err != nil {
			return nil, fmt.Errorf("desired state: %w", err)
		}
		if _, twice := want.instances[name]; twice {
			return nil, fmt.Errorf("desired state names instance %q twice", entry.Name)
		}
		for own := range l.cfg.Env {
			if _, set := entry.Env[own]; set {
				return nil, fmt.Errorf("desired instance %q sets %s, which this launcher adds from its own config", entry.Name, own)
			}
		}
		want.instances[name] = instance{name: name, image: entry.Image, env: entry.Env, endsAt: entry.EndsAt}
	}
	if answer.Refusal != nil {
		if answer.Refusal.Reason == "" || answer.Refusal.Message == "" {
			return nil, errors.New("answer carries a refusal with no reason or no message")
		}
		want.refusal = answer.Refusal.Reason + ": " + answer.Refusal.Message
	}
	for _, short := range *answer.Ending {
		name := l.prefix + short
		if short == "" {
			return nil, errors.New("desired state names an ending instance with no name")
		}
		if _, wanted := want.instances[name]; wanted {
			return nil, fmt.Errorf("desired state names instance %q as both running and ending", short)
		}
		want.ending[name] = true
	}
	return want, nil
}
