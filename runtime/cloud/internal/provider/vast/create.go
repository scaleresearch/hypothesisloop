package vast

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

const listedTimeout = 30 * time.Second

// The API takes env as one JSON object shared with container options: a key such as "-p 80:80"
// publishes a port. Only real variable names may pass.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type createRequest struct {
	ClientID string `json:"client_id"`
	Image    string `json:"image"`
	// RunType "args" runs the image as is: no ssh or jupyter wrapper replaces its entrypoint.
	RunType string            `json:"runtype"`
	Label   string            `json:"label"`
	Disk    int64             `json:"disk"`
	Env     map[string]string `json:"env"`
	// OnStart is the entrypoint override under run type "args", one executable and no arguments.
	OnStart string   `json:"onstart"`
	Args    []string `json:"args"`
}

func (p *Provider) Create(ctx context.Context, spec provider.Spec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	chosen, err := p.cheapestOffer(ctx, spec)
	if err != nil {
		return "", err
	}
	var created struct {
		Success  bool   `json:"success"`
		Contract int64  `json:"new_contract"`
		Message  string `json:"msg"`
	}
	path := "/api/v0/asks/" + strconv.FormatInt(chosen.ID, 10) + "/"
	if _, err := p.call(ctx, http.MethodPut, path, newCreateRequest(spec), &created); err != nil {
		return "", err
	}
	if !created.Success || created.Contract == 0 {
		return "", fmt.Errorf("vast: renting offer %d for %q was refused: %s", chosen.ID, spec.Name, created.Message)
	}
	id := strconv.FormatInt(created.Contract, 10)
	if err := p.awaitListed(ctx, id, spec.Name); err != nil {
		return "", p.giveBack(ctx, id, err)
	}
	return id, nil
}

// giveBack destroys an instance Create rented and could not confirm, so that a failed Create
// leaves nothing behind. ctx may be what ended the wait, so the destroy runs on its own time.
func (p *Provider) giveBack(ctx context.Context, id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
	defer cancel()
	if err := p.Destroy(ctx, id); err != nil {
		return fmt.Errorf("%w; destroying it failed and it may still be rented: %v", cause, err)
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

// awaitListed holds Create until the rented instance is in List, which is what tells a caller
// with no memory of this call that the name is taken.
func (p *Provider) awaitListed(ctx context.Context, id, name string) error {
	ctx, cancel := context.WithTimeout(ctx, listedTimeout)
	defer cancel()
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		listed, err := p.listed(ctx, id, name)
		if err != nil {
			return fmt.Errorf("vast: instance %s was rented for %q and could not be confirmed in the list: %w", id, name, err)
		}
		if listed {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("vast: instance %s was rented for %q and is not in the list: %w", id, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// newCreateRequest hands the argv over as JSON strings end to end, so no shell ever parses it.
func newCreateRequest(spec provider.Spec) createRequest {
	entrypoint, args := supervised(spec.Command)
	req := createRequest{
		ClientID: "me",
		Image:    spec.Image,
		RunType:  "args",
		Label:    spec.Name,
		Disk:     diskGB(spec),
		Env:      spec.Env,
		OnStart:  entrypoint,
		Args:     args,
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	return req
}

func validateSpec(spec provider.Spec) error {
	if err := provider.ValidateName(spec.Name); err != nil {
		return fmt.Errorf("vast: %w", err)
	}
	if spec.Image == "" {
		return fmt.Errorf("vast: spec %q has no image", spec.Name)
	}
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return fmt.Errorf("vast: spec %q has no command", spec.Name)
	}
	if spec.AcceleratorCount < 0 {
		return fmt.Errorf("vast: spec %q asks for %d accelerators", spec.Name, spec.AcceleratorCount)
	}
	if spec.CPUMillis <= 0 || spec.MemoryBytes <= 0 || spec.DiskBytes <= 0 {
		return fmt.Errorf("vast: spec %q must state cpu, memory and disk", spec.Name)
	}
	for name := range spec.Env {
		if !envName.MatchString(name) {
			return fmt.Errorf("vast: spec %q: %q is not an environment variable name", spec.Name, name)
		}
	}
	return nil
}
