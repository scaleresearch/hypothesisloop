package fly

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
)

type machine struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Config    struct {
		Env  map[string]string `json:"env"`
		Init machineInit       `json:"init"`
	} `json:"config"`
}

// listParallel bounds the apps whose Machines are read at once. Each app is its own budget of
// the API's rate limit.
const listParallel = 8

// List reads the organization's apps and the Machines of each of this adapter's, side by side.
// One read that fails fails the list, since the Machine it would show is not known to be gone.
func (p *Provider) List(ctx context.Context, namePrefix string) ([]provider.Instance, error) {
	apps, err := p.apps(ctx)
	if err != nil {
		return nil, err
	}
	found := make([][]provider.Instance, len(apps))
	failures := make([]error, len(apps))
	slots := make(chan struct{}, listParallel)
	var reading sync.WaitGroup
	for i, app := range apps {
		reading.Add(1)
		go func() {
			defer reading.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			found[i], failures[i] = p.appInstances(ctx, app, namePrefix)
		}()
	}
	reading.Wait()
	if err := errors.Join(failures...); err != nil {
		return nil, err
	}
	listed := []provider.Instance{}
	for _, instances := range found {
		listed = append(listed, instances...)
	}
	sort.Slice(listed, func(a, b int) bool { return listed[a].ID < listed[b].ID })
	return listed, nil
}

// appInstances reads the Machines of one app. An app deleted since the app list was read has
// none.
func (p *Provider) appInstances(ctx context.Context, app, namePrefix string) ([]provider.Instance, error) {
	var machines []machine
	header, err := p.call(ctx, http.MethodGet, p.appPath(app)+"/machines", nil, &machines)
	if hasStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return nil, fmt.Errorf("fly: the machine list of app %s carries no usable Date: %w", app, err)
	}
	listed := []provider.Instance{}
	for _, found := range machines {
		name, named := found.Config.Env[nameEnv]
		if !named || !strings.HasPrefix(name, namePrefix) {
			continue
		}
		state, exists := p.stateOf(ctx, app, found, now)
		if !exists {
			continue
		}
		listed = append(listed, provider.Instance{ID: instanceID(app, found.ID), Name: name, State: state, StartedAt: found.CreatedAt, Architecture: architecture})
	}
	return listed, nil
}

// Get reads the Machine from its host's own record. The list is a copy that trails it: on
// 2026-10-09 it left out running Machines for over a minute. The API answers 404 for an id it
// never had, and a destroyed Machine for a while after. An id outside this adapter's apps names
// no Machine of it, which is gone to the caller as one the vendor never had.
func (p *Provider) Get(ctx context.Context, id string) (provider.Instance, bool, error) {
	app, machineID, err := p.splitID(id)
	if err != nil {
		return provider.Instance{}, false, nil
	}
	var found machine
	header, err := p.call(ctx, http.MethodGet, p.machinePath(app, machineID, ""), nil, &found)
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
	state, exists := p.stateOf(ctx, app, found, now)
	if !exists {
		return provider.Instance{}, false, nil
	}
	return provider.Instance{ID: id, Name: found.Config.Env[nameEnv], State: state, StartedAt: found.CreatedAt, Architecture: architecture}, true, nil
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
func (p *Provider) stateOf(ctx context.Context, app string, found machine, now time.Time) (state provider.State, exists bool) {
	if slices.Equal(found.Config.Init.Exec, idle) {
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
		if p.outputComplete(ctx, app, found, now) {
			return provider.StateExited, true
		}
		return provider.StateRunning, true
	case "destroying", "destroyed":
		return "", false
	}
	p.log("machine %s (%s) has state %q, which this adapter does not know; reporting it as unknown", found.ID, found.Config.Env[nameEnv], found.State)
	return provider.StateUnknown, true
}

// outputComplete tells whether everything a stopped Machine's command wrote is in the log: the
// supervisor's end mark is there, or the Machine stopped longer ago than the log ever trails.
// The second covers a Machine that stopped without its supervisor finishing (the host killed it,
// it ran out of memory, the image has no shell), which writes no mark and nothing more either.
// The end mark of an earlier start of the Machine says nothing of this one, so only what follows
// the newest start mark is searched.
func (p *Provider) outputComplete(ctx context.Context, app string, found machine, now time.Time) bool {
	if now.Sub(found.UpdatedAt) > logSettle {
		return true
	}
	entries, err := p.logEntries(ctx, app, found.ID, endMarkWindow)
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

// Destroy deletes the Machine's app, and the Machine with it, whatever its state.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	app, _, err := p.splitID(id)
	if err != nil {
		return err
	}
	return p.deleteApp(ctx, app)
}
