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
	"sync"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/vendorapi"
)

const (
	listPageSize = 100

	// stateReaders is how many sandboxes List reads at a time. One after another, fifty
	// sandboxes took eighteen seconds.
	stateReaders = 8

	// stateReadWindow bounds the two calls that read one sandbox, so that one that does not
	// answer costs a pass this long and no longer.
	stateReadWindow = 10 * time.Second

	// maxStateBytes is more than the supervisor ever writes to the state file, which is two
	// short lines.
	maxStateBytes = 64
)

type sandbox struct {
	SandboxID  string            `json:"sandboxID"`
	State      string            `json:"state"`
	StartedAt  time.Time         `json:"startedAt"`
	Metadata   map[string]string `json:"metadata"`
	CPUCount   int64             `json:"cpuCount"`
	MemoryMB   int64             `json:"memoryMB"`
	DiskSizeMB int64             `json:"diskSizeMB"`
}

// List fails only when the vendor's own list does. Every sandbox that list shows under the
// prefix is returned, whatever reading its state came to: a caller creates what it does not
// find, so one left out would be rented twice.
func (p *Provider) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	sandboxes, err := p.sandboxes(ctx, namePrefix)
	if err != nil {
		return nil, err
	}
	listed := make([]provider.Instance, len(sandboxes))
	readers := make(chan struct{}, stateReaders)
	var reading sync.WaitGroup
	for i, found := range sandboxes {
		reading.Add(1)
		go func() {
			defer reading.Done()
			readers <- struct{}{}
			defer func() { <-readers }()
			listed[i] = provider.Instance{ID: found.SandboxID, Name: found.Metadata[nameKey], State: p.stateOf(ctx, found), StartedAt: found.StartedAt}
		}()
	}
	reading.Wait()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("e2b: list: the states of the sandboxes could not all be read in time: %w", err)
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

// stateOf reads what the supervisor wrote inside the sandbox, and always has an answer:
//
//   - No state file: the command has not started. Its Create is still under way, or broke off
//     before the start.
//   - A read that failed (the sandbox's token or its envd did not answer, or answered with a
//     refusal): running. The vendor says the sandbox exists, nothing says its command ended, and
//     the next pass reads again.
//   - A state file the supervisor did not write: exited. The file is the command's user's to
//     change, so the supervisor's word is gone for good and the caller should replace the
//     instance.
//   - Paused: starting. This adapter never pauses, so somebody did by hand; the sandbox keeps its
//     name, can be destroyed, and the caller's boot timeout bounds it.
func (p *Provider) stateOf(ctx context.Context, found sandbox) provider.State {
	id, name := found.SandboxID, found.Metadata[nameKey]
	if found.State != "running" {
		log.Printf("e2b: sandbox %s (%s) has state %q, which this adapter never asks for; reporting it as starting", id, name, found.State)
		return provider.StateStarting
	}
	ctx, cancel := context.WithTimeout(ctx, p.stateRead)
	defer cancel()
	inside, exists, err := p.inside(ctx, id)
	if err != nil {
		log.Printf("e2b: the token of sandbox %s (%s) could not be read; reporting it as running: %v", id, name, err)
		return provider.StateRunning
	}
	if !exists {
		log.Printf("e2b: sandbox %s (%s) is in the list and was not found when read; reporting it as running", id, name)
		return provider.StateRunning
	}
	written, started, err := inside.readFile(ctx, statePath, maxStateBytes)
	if errors.Is(err, vendorapi.ErrTooLarge) {
		log.Printf("e2b: sandbox %s (%s): its state file is larger than the %d bytes the supervisor writes at most; reporting it as exited", id, name, maxStateBytes)
		return provider.StateExited
	}
	if err != nil {
		log.Printf("e2b: the state of sandbox %s (%s) could not be read; reporting it as running: %v", id, name, err)
		return provider.StateRunning
	}
	if !started {
		return provider.StateStarting
	}
	state, err := stateWritten(written)
	if err != nil {
		log.Printf("e2b: sandbox %s (%s): %v; reporting it as exited", id, name, err)
		return provider.StateExited
	}
	return state
}

// stateWritten maps the state file: one line when the command started, a second when it ended.
func stateWritten(written string) (provider.State, error) {
	switch written {
	case "":
		return provider.StateStarting, nil
	case stateRunning + "\n":
		return provider.StateRunning, nil
	case stateRunning + "\n" + stateExited + "\n":
		return provider.StateExited, nil
	}
	return "", fmt.Errorf("its state file holds %q, which the supervisor does not write", written)
}

// inside returns the envd of a sandbox, and exists false when the sandbox is gone. The token
// envd asks for is the sandbox's own and is read from the API on every use.
func (p *Provider) inside(ctx context.Context, id string) (inside envd, exists bool, err error) {
	var detail struct {
		EnvdAccessToken string `json:"envdAccessToken"`
	}
	_, err = p.call(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, &detail)
	if hasStatus(err, http.StatusNotFound) {
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

func (p *Provider) Destroy(ctx context.Context, id string) error {
	_, err := p.call(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
