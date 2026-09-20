package e2b

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const listPageSize = 100

type sandbox struct {
	SandboxID  string            `json:"sandboxID"`
	State      string            `json:"state"`
	StartedAt  time.Time         `json:"startedAt"`
	Metadata   map[string]string `json:"metadata"`
	CPUCount   int64             `json:"cpuCount"`
	MemoryMB   int64             `json:"memoryMB"`
	DiskSizeMB int64             `json:"diskSizeMB"`
}

func (p *Provider) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	sandboxes, err := p.sandboxes(ctx, namePrefix)
	if err != nil {
		return nil, err
	}
	listed := []provider.Instance{}
	for _, found := range sandboxes {
		state, exists, err := p.stateOf(ctx, found)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		listed = append(listed, provider.Instance{ID: found.SandboxID, Name: found.Metadata[nameKey], State: state, StartedAt: found.StartedAt})
	}
	return listed, nil
}

// sandboxes returns the team's sandboxes whose name starts with namePrefix. The API filters on a
// whole metadata value and has no prefix match, so every page is read.
func (p *Provider) sandboxes(ctx context.Context, namePrefix string) ([]sandbox, error) {
	var matching []sandbox
	token := ""
	for {
		query := url.Values{"state": {"running,paused"}, "limit": {strconv.Itoa(listPageSize)}}
		if token != "" {
			query.Set("nextToken", token)
		}
		var page []sandbox
		header, err := p.call(ctx, http.MethodGet, "/v2/sandboxes?"+query.Encode(), nil, &page)
		if err != nil {
			return nil, err
		}
		for _, found := range page {
			if name, named := found.Metadata[nameKey]; named && strings.HasPrefix(name, namePrefix) {
				matching = append(matching, found)
			}
		}
		token = header.Get("X-Next-Token")
		if token == "" {
			return matching, nil
		}
	}
}

// stateOf reads what the supervisor wrote inside the sandbox. A sandbox with no state file has
// not started its command: its Create is still under way, or broke off before the start. A paused
// sandbox is one somebody paused by hand, since this adapter never pauses; it is starting as
// well, and not known: it stays listed, so it keeps its name and can be destroyed, and the
// caller's boot timeout bounds it. A sandbox that ended between the list and the read is gone.
func (p *Provider) stateOf(ctx context.Context, found sandbox) (state provider.State, exists bool, err error) {
	if found.State != "running" {
		log.Printf("e2b: sandbox %s (%s) has state %q, which this adapter never asks for; reporting it as starting", found.SandboxID, found.Metadata[nameKey], found.State)
		return provider.StateStarting, true, nil
	}
	inside, exists, err := p.inside(ctx, found.SandboxID)
	if err != nil || !exists {
		return "", false, err
	}
	written, started, err := inside.readFile(ctx, statePath)
	if err != nil {
		return "", false, fmt.Errorf("e2b: read the state of sandbox %s (%s): %w", found.SandboxID, found.Metadata[nameKey], err)
	}
	if !started {
		return provider.StateStarting, true, nil
	}
	state, err = stateWritten(written)
	if err != nil {
		return "", false, fmt.Errorf("e2b: sandbox %s (%s): %w", found.SandboxID, found.Metadata[nameKey], err)
	}
	return state, true, nil
}

// stateWritten maps the state file: one line when the command started, a second when it ended.
func stateWritten(written string) (provider.State, error) {
	lines := strings.Split(strings.TrimSuffix(written, "\n"), "\n")
	switch {
	case written == "":
		return provider.StateStarting, nil
	case len(lines) == 1 && lines[0] == stateRunning:
		return provider.StateRunning, nil
	case len(lines) == 2 && lines[0] == stateRunning && strings.HasPrefix(lines[1], stateExited+" "):
		return provider.StateExited, nil
	}
	return "", fmt.Errorf("its state file holds %q, which the supervisor does not write", truncate([]byte(written)))
}

// inside returns the envd of a sandbox, and exists false when the sandbox is gone. The token
// envd asks for is the sandbox's own and is read from the API on every use.
func (p *Provider) inside(ctx context.Context, id string) (inside envd, exists bool, err error) {
	var detail struct {
		EnvdAccessToken string `json:"envdAccessToken"`
	}
	_, err = p.call(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, &detail)
	if isGone(err) {
		return envd{}, false, nil
	}
	if err != nil {
		return envd{}, false, err
	}
	if detail.EnvdAccessToken == "" {
		return envd{}, false, fmt.Errorf("e2b: sandbox %s was shown without the token its envd asks for", id)
	}
	return envd{p: p, base: p.envdURL(id), token: detail.EnvdAccessToken}, true, nil
}

func isGone(err error) bool {
	var failure *statusError
	return errors.As(err, &failure) && failure.Status == http.StatusNotFound
}

func (p *Provider) Destroy(ctx context.Context, id string) error {
	_, err := p.call(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil)
	if isGone(err) {
		return nil
	}
	return err
}
