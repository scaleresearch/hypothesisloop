package fly

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const listedTimeout = 30 * time.Second

// nameKey is the metadata key a Machine carries its instance name under. The Machine's own name
// is left to the vendor: it must be unique in the app, and an instance name need not be.
const nameKey = "hl_name"

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type createRequest struct {
	Region string        `json:"region"`
	Config machineConfig `json:"config"`
}

type machineConfig struct {
	Image    string            `json:"image"`
	Init     machineInit       `json:"init"`
	Env      map[string]string `json:"env"`
	Metadata map[string]string `json:"metadata"`
	Guest    guest             `json:"guest"`
	Rootfs   machineRootfs     `json:"rootfs"`
	// Restart "no" and AutoDestroy false are what keep a Machine whose command ended in the
	// list, stopped, with its logs readable.
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
	var created struct {
		ID string `json:"id"`
	}
	if _, err := p.call(ctx, http.MethodPost, p.machinesPath(""), req, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("fly: creating a machine for %q answered without an id", spec.Name)
	}
	if err := p.awaitListed(ctx, created.ID, spec.Name); err != nil {
		return "", p.giveBack(ctx, created.ID, err)
	}
	return created.ID, nil
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
	env := spec.Env
	if env == nil {
		env = map[string]string{}
	}
	return createRequest{
		Region: p.cfg.Region,
		Config: machineConfig{
			Image:    spec.Image,
			Init:     machineInit{Exec: supervised(spec.Command)},
			Env:      env,
			Metadata: map[string]string{nameKey: spec.Name},
			Guest:    size,
			Rootfs:   machineRootfs{SizeGB: disk},
			Restart:  machineRestart{Policy: "no"},
		},
	}, nil
}

// giveBack destroys a Machine Create made and could not confirm, so that a failed Create leaves
// nothing behind. ctx may be what ended the wait, so the destroy runs on its own time.
func (p *Provider) giveBack(ctx context.Context, id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
	defer cancel()
	if err := p.Destroy(ctx, id); err != nil {
		return fmt.Errorf("%w; destroying it failed and it may still be billed: %v", cause, err)
	}
	return fmt.Errorf("%w; it was destroyed", cause)
}

func (p *Provider) listed(ctx context.Context, id, name string) (bool, error) {
	instances, err := p.List(ctx, name)
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
// memory of this call that the name is taken.
func (p *Provider) awaitListed(ctx context.Context, id, name string) error {
	ctx, cancel := context.WithTimeout(ctx, listedTimeout)
	defer cancel()
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		listed, err := p.listed(ctx, id, name)
		if err != nil {
			return fmt.Errorf("fly: machine %s was created for %q and could not be confirmed in the list: %w", id, name, err)
		}
		if listed {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("fly: machine %s was created for %q and is not in the list: %w", id, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateSpec(spec provider.Spec) error {
	if err := provider.ValidateName(spec.Name); err != nil {
		return fmt.Errorf("fly: %w", err)
	}
	if spec.Image == "" {
		return fmt.Errorf("fly: spec %q has no image", spec.Name)
	}
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return fmt.Errorf("fly: spec %q has no command", spec.Name)
	}
	if spec.AcceleratorCount != 0 {
		return fmt.Errorf("fly: spec %q asks for %d accelerators of type %q, and this adapter rents none", spec.Name, spec.AcceleratorCount, spec.AcceleratorType)
	}
	if spec.CPUMillis <= 0 || spec.MemoryBytes <= 0 || spec.DiskBytes <= 0 {
		return fmt.Errorf("fly: spec %q must state cpu, memory and disk", spec.Name)
	}
	for name := range spec.Env {
		if !envName.MatchString(name) {
			return fmt.Errorf("fly: spec %q: %q is not an environment variable name", spec.Name, name)
		}
	}
	return nil
}
