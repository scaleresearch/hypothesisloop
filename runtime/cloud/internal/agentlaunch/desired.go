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

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// instance is one entry of the control plane's list under the name the provider will hold it by.
type instance struct {
	name  string
	image string
	env   map[string]string
}

// desired is one answer of the control plane, keyed by full instance name: what should run, and
// what has just stopped being wanted and is left to end by itself.
type desired struct {
	instances map[string]instance
	ending    map[string]bool
}

// reconcileAnswer is the wire shape. Both lists are pointers because an answer without one is
// not an empty list: it is an answer this launcher cannot read, and acting on it would destroy
// every agent.
type reconcileAnswer struct {
	Instances *[]struct {
		Name  string            `json:"name"`
		Image string            `json:"image"`
		Env   map[string]string `json:"env"`
	} `json:"instances"`
	Ending *[]string `json:"ending"`
}

// maxAnswerBytes bounds what one answer may hold in memory.
const maxAnswerBytes = 64 << 20

// observedInstance is one name of this launcher's namespace as its provider lists it, without
// the launcher's prefix.
type observedInstance struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Log   string `json:"log,omitempty"`
}

// reconcileQuestion is what the launcher tells the control plane with every question.
type reconcileQuestion struct {
	LaunchDigest string             `json:"launch_digest"`
	Instances    []observedInstance `json:"instances"`
}

func (l *Launcher) fetchDesired(ctx context.Context, observed []observedInstance) (*desired, error) {
	question, err := json.Marshal(reconcileQuestion{LaunchDigest: l.launchDigest, Instances: observed})
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
		if entry.Name == "" || entry.Image == "" {
			return nil, fmt.Errorf("desired instance %q has no name or no image", entry.Name)
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
		want.instances[name] = instance{name: name, image: entry.Image, env: entry.Env}
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
