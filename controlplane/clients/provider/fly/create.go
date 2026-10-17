package fly

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/vendorapi"
)

// nameEnv is the variable of a Machine's env that holds its instance name, where the Machine
// cannot change it (apps.go). The Machine's own name is fixed at create, and a warm Machine takes
// another instance name when it starts.
const nameEnv = "HL_INSTANCE_NAME"

type createRequest struct {
	Region string        `json:"region"`
	Config machineConfig `json:"config"`
	// SkipLaunch leaves a created Machine stopped once its image is ready, and an updated one
	// stopped whatever it was (warm.go).
	SkipLaunch bool `json:"skip_launch,omitempty"`
}

type machineConfig struct {
	Image  string            `json:"image"`
	Init   machineInit       `json:"init"`
	Env    map[string]string `json:"env"`
	Guest  guest             `json:"guest"`
	Rootfs machineRootfs     `json:"rootfs"`
	// Restart "no" and AutoDestroy false are what keep a Machine whose command ended in the
	// list, stopped, with its logs readable: the end of its output is how a caller learns why
	// it ended, and a Machine the vendor destroyed could not be told from one it took away.
	Restart     machineRestart `json:"restart"`
	AutoDestroy bool           `json:"auto_destroy"`
}

type machineInit struct {
	// Exec replaces the image's entrypoint and arguments with one argv.
	Exec []string `json:"exec"`
}

type machineRootfs struct {
	SizeGB int64 `json:"size_gb"`
}

type machineRestart struct {
	Policy string `json:"policy"`
}

func (p *Provider) Create(ctx context.Context, spec provider.Spec) (string, error) {
	req, err := p.newCreateRequest(spec)
	if err != nil {
		return "", err
	}
	return p.rent(ctx, req, spec.Name)
}

// rent creates an app and req's Machine in it, and holds until the Machine is listed. The app
// goes again when its Machine is not made or not confirmed.
func (p *Provider) rent(ctx context.Context, req createRequest, name string) (string, error) {
	app, err := p.createApp(ctx)
	if err != nil {
		return "", err
	}
	giveBack := func(cause error) error {
		deleteApp := func(ctx context.Context) error { return p.deleteApp(ctx, app) }
		return vendorapi.GiveBack(ctx, deleteApp, requestTimeout, cause)
	}
	var created struct {
		ID string `json:"id"`
	}
	if _, err := p.call(ctx, http.MethodPost, p.appPath(app)+"/machines", req, &created); err != nil {
		return "", giveBack(err)
	}
	if !machineIDPattern.MatchString(created.ID) {
		return "", giveBack(fmt.Errorf("fly: creating a machine for %q answered without an id", name))
	}
	id := instanceID(app, created.ID)
	if err := p.awaitListed(ctx, id, name); err != nil {
		return "", giveBack(err)
	}
	return id, nil
}

// newCreateRequest hands the argv over as JSON strings end to end, so no shell ever parses it.
func (p *Provider) newCreateRequest(spec provider.Spec) (createRequest, error) {
	if err := validateSpec(spec); err != nil {
		return createRequest{}, err
	}
	size, err := p.cfg.guestFor(spec)
	if err != nil {
		return createRequest{}, err
	}
	disk, err := rootfsGB(spec)
	if err != nil {
		return createRequest{}, err
	}
	env := map[string]string{nameEnv: spec.Name}
	for key, value := range spec.Env {
		env[key] = value
	}
	return createRequest{
		Region: p.cfg.Region,
		Config: machineConfig{
			Image:   spec.Image,
			Init:    machineInit{Exec: supervised(spec.Command)},
			Env:     env,
			Guest:   size,
			Rootfs:  machineRootfs{SizeGB: disk},
			Restart: machineRestart{Policy: "no"},
		},
	}, nil
}

// listed reads the app list and the Machines of id's app alone.
func (p *Provider) listed(ctx context.Context, id, name string) (bool, error) {
	app, _, err := p.splitID(id)
	if err != nil {
		return false, err
	}
	apps, err := p.apps(ctx)
	if err != nil || !slices.Contains(apps, app) {
		return false, err
	}
	instances, err := p.appInstances(ctx, app, name)
	if err != nil {
		return false, err
	}
	for _, found := range instances {
		if found.ID == id && found.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// awaitListed holds Create until the Machine is in List, which is what tells a caller with no
// memory of this call that the name is taken. A list that fails is read again, since one refused
// call says nothing about the Machine, and the Machine is given up only when the window ends
// without a sighting.
func (p *Provider) awaitListed(ctx context.Context, id, name string) error {
	ctx, cancel := context.WithTimeout(ctx, p.confirm)
	defer cancel()
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	var failed error
	for {
		listed, err := p.listed(ctx, id, name)
		if listed {
			return nil
		}
		if ctx.Err() == nil {
			failed = err
		}
		select {
		case <-ctx.Done():
			if failed != nil {
				return fmt.Errorf("fly: machine %s was created for %q and could not be confirmed in the list: %w", id, name, failed)
			}
			return fmt.Errorf("fly: machine %s was created for %q and is not in the list: %w", id, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateSpec(spec provider.Spec) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("fly: %w", err)
	}
	if spec.Architecture != architecture {
		return fmt.Errorf("fly: spec %q runs on %s, and this adapter rents %s machines only", spec.Name, spec.Architecture, architecture)
	}
	if _, set := spec.Env[nameEnv]; set {
		return fmt.Errorf("fly: spec %q sets %s, which holds the instance name", spec.Name, nameEnv)
	}
	if spec.ImageLogin != nil {
		return fmt.Errorf("fly: spec %q pulls its image with a login, and this adapter pulls only public images", spec.Name)
	}
	if spec.AcceleratorCount != 0 {
		return fmt.Errorf("fly: spec %q asks for %d accelerators of type %q, and this adapter rents none", spec.Name, spec.AcceleratorCount, spec.AcceleratorType)
	}
	return nil
}
