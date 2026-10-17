package fly

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/provider"
)

type machine struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Config    struct {
		Metadata map[string]string `json:"metadata"`
	} `json:"config"`
}

// List reads every Machine of the app in one call: the API filters on a whole metadata value and
// has no prefix match, and without a page size it answers with all of them.
func (p *Provider) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	var machines []machine
	header, err := p.call(ctx, http.MethodGet, p.machinesPath(""), nil, &machines)
	if hasStatus(err, http.StatusNotFound) {
		return nil, fmt.Errorf("fly: app %q does not exist, or the token is for another app; the adapter does not create it: %w", p.cfg.App, err)
	}
	if err != nil {
		return nil, err
	}
	now, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return nil, fmt.Errorf("fly: the machine list carries no usable Date: %w", err)
	}
	listed := []provider.Instance{}
	for _, found := range machines {
		name, named := found.Config.Metadata[nameKey]
		if !named || !strings.HasPrefix(name, namePrefix) {
			continue
		}
		state, exists := p.stateOf(ctx, found, now)
		if !exists {
			continue
		}
		listed = append(listed, provider.Instance{ID: found.ID, Name: name, State: state, StartedAt: found.CreatedAt})
	}
	return listed, nil
}

// Get reads the Machine from its host's own record. The list is a copy that trails it: on
// 2026-10-09 it left out running Machines for over a minute. The API answers 404 for an id it
// never had, and a destroyed Machine for a while after.
func (p *Provider) Get(ctx context.Context, id string) (provider.Instance, bool, error) {
	var found machine
	header, err := p.call(ctx, http.MethodGet, p.machinesPath("/"+url.PathEscape(id)), nil, &found)
	if hasStatus(err, http.StatusNotFound) {
		return provider.Instance{}, false, nil
	}
	if err != nil {
		return provider.Instance{}, false, err
	}
	now, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return provider.Instance{}, false, fmt.Errorf("fly: the machine read carries no usable Date: %w", err)
	}
	state, exists := p.stateOf(ctx, found, now)
	if !exists {
		return provider.Instance{}, false, nil
	}
	return provider.Instance{ID: found.ID, Name: found.Config.Metadata[nameKey], State: state, StartedAt: found.CreatedAt}, true, nil
}

// stateOf maps a Machine's state. A Machine being destroyed is gone to a caller. One the vendor
// restarts, updates or replaces has run its command and is not known to run it now, and a state
// this adapter does not know says nothing either: both are unknown, and the Machine stays
// listed, so it keeps its name and can be destroyed.
//
// A stopped Machine is exited only once its output is complete, and until then it is running:
// the log trails the Machine by a moment, and the contract lets a caller read the last lines
// once, at the first look that shows exited.
//
// A warm Machine is stopped once its image is ready and has written nothing, so no log is read
// for it. One that failed is exited, and one in any other state went on as any Machine does.
func (p *Provider) stateOf(ctx context.Context, found machine, now time.Time) (state provider.State, exists bool) {
	if found.Config.Metadata[warmKey] != "" {
		switch found.State {
		case "stopped":
			return provider.StateWarm, true
		case "failed":
			return provider.StateExited, true
		}
	}
	switch found.State {
	case "created", "creating", "starting":
		return provider.StateStarting, true
	case "restarting", "updating", "replacing":
		return provider.StateUnknown, true
	case "started", "stopping", "suspending":
		return provider.StateRunning, true
	case "suspended":
		return provider.StatePaused, true
	case "stopped", "failed":
		if p.outputComplete(ctx, found, now) {
			return provider.StateExited, true
		}
		return provider.StateRunning, true
	case "destroying", "destroyed":
		return "", false
	}
	p.log("machine %s (%s) has state %q, which this adapter does not know; reporting it as unknown", found.ID, found.Config.Metadata[nameKey], found.State)
	return provider.StateUnknown, true
}

// outputComplete tells whether everything a stopped Machine's command wrote is in the log: the
// supervisor's end mark is there, or the Machine stopped longer ago than the log ever trails.
// The second covers a Machine that stopped without its supervisor finishing (the host killed it,
// it ran out of memory, the image has no shell), which writes no mark and nothing more either.
// The end mark of an earlier start of the Machine says nothing of this one, so only what follows
// the newest start mark is searched.
func (p *Provider) outputComplete(ctx context.Context, found machine, now time.Time) bool {
	if now.Sub(found.UpdatedAt) > logSettle {
		return true
	}
	entries, err := p.logEntries(ctx, found.ID, endMarkWindow)
	if err != nil {
		p.log("machine %s stopped and its log could not be read for the end mark; reporting it as running: %v", found.ID, err)
		return false
	}
	run, _ := lastRun(entries)
	for _, entry := range run {
		if entry == endMark {
			return true
		}
	}
	return false
}

// Destroy deletes the Machine whatever its state. The API answers 200 for a Machine it deleted
// before and 404 for an id it never had.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	var destroyed struct {
		OK bool `json:"ok"`
	}
	_, err := p.call(ctx, http.MethodDelete, p.machinesPath("/"+url.PathEscape(id))+"?force=true", nil, &destroyed)
	if hasStatus(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !destroyed.OK {
		return fmt.Errorf("fly: destroying machine %s was refused", id)
	}
	return nil
}
