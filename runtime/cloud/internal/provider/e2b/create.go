package e2b

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

// launchTimeout bounds what Create does inside a sandbox it has made and the wait for the
// sandbox to be listed.
const launchTimeout = 30 * time.Second

// nameKey is the metadata key a sandbox carries its instance name under.
const nameKey = "hl_name"

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type createRequest struct {
	TemplateID string `json:"templateID"`
	// Timeout is the sandbox's whole lifetime in seconds. With AutoPause false the vendor kills
	// the sandbox when it ends.
	Timeout   int               `json:"timeout"`
	AutoPause bool              `json:"autoPause"`
	Metadata  map[string]string `json:"metadata"`
}

// Create starts a sandbox from the spec's template and then the command inside it. A sandbox
// whose command could not be started, or that is smaller than the spec, is killed again.
func (p *Provider) Create(ctx context.Context, spec provider.Spec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	template := templateName(spec.Image, sizeOf(spec))
	var created struct {
		SandboxID       string `json:"sandboxID"`
		EnvdAccessToken string `json:"envdAccessToken"`
	}
	req := createRequest{TemplateID: template, Timeout: p.cfg.LifetimeSeconds, Metadata: map[string]string{nameKey: spec.Name}}
	_, err := p.call(ctx, http.MethodPost, "/v2/sandboxes", req, &created)
	var failure *statusError
	if errors.As(err, &failure) && failure.Status == http.StatusNotFound {
		size := sizeOf(spec)
		return "", fmt.Errorf("e2b: template %s for image %q with %d cores and %d MiB of memory is not built; build it with `%s %s %d %d`: %w",
			template, spec.Image, size.CPUs, size.MemoryMB, templateScript, spec.Image, size.CPUs, size.MemoryMB, err)
	}
	if err != nil {
		return "", err
	}
	if created.SandboxID == "" || created.EnvdAccessToken == "" {
		return "", fmt.Errorf("e2b: creating a sandbox for %q answered without an id or without its token", spec.Name)
	}
	if err := p.launch(ctx, created.SandboxID, created.EnvdAccessToken, spec, template); err != nil {
		return "", p.giveBack(ctx, created.SandboxID, err)
	}
	return created.SandboxID, nil
}

// launch starts the command in a fresh sandbox, sees the supervisor write its state, and waits
// for the sandbox to be in List, which is what tells a caller with no memory of this call that
// the name is taken.
func (p *Provider) launch(ctx context.Context, id, token string, spec provider.Spec, template string) error {
	ctx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()
	inside := envd{p: p, base: p.envdURL(id), token: token}
	if err := inside.writeFile(ctx, launchPath, launchFile(spec.Command, spec.Env)); err != nil {
		return fmt.Errorf("e2b: sandbox %s was created for %q and its command could not be uploaded: %w", id, spec.Name, err)
	}
	start := startRequest{Process: processConfig{Cmd: "/bin/sh", Args: []string{launchPath}}, Tag: supervisorTag}
	if err := inside.start(ctx, start, func(processEvent) bool { return false }); err != nil {
		return fmt.Errorf("e2b: sandbox %s was created for %q and its command could not be started: %w", id, spec.Name, err)
	}
	if err := p.awaitSupervisor(ctx, inside, id, spec.Name, template); err != nil {
		return err
	}
	listed, err := p.awaitListed(ctx, id, spec.Name)
	if err != nil {
		return err
	}
	asked := sizeOf(spec)
	if disk := ceilDiv(spec.DiskBytes, bytesPerMB); listed.CPUCount < asked.CPUs || listed.MemoryMB < asked.MemoryMB || listed.DiskSizeMB < disk {
		return fmt.Errorf("e2b: template %s gives a sandbox %d cores, %d MiB of memory and %d MiB of disk, and spec %q asks for %d, %d and %d",
			template, listed.CPUCount, listed.MemoryMB, listed.DiskSizeMB, spec.Name, asked.CPUs, asked.MemoryMB, disk)
	}
	return nil
}

// awaitSupervisor waits for the state file, the first thing the supervisor writes. envd reports
// a process as started before the shell has read a line, so only the file shows that the launch
// file ran as far as the supervisor.
func (p *Provider) awaitSupervisor(ctx context.Context, inside envd, id, name, template string) error {
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		_, written, err := inside.readFile(ctx, statePath)
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("e2b: sandbox %s was created for %q and its supervisor could not be seen starting: %w", id, name, err)
		}
		if written {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("e2b: sandbox %s was created for %q and its supervisor did not start; it needs sh, %s as %s writes it into template %s, and a %s it can write to: %w",
				id, name, imageEnvPath, templateScript, template, superviseDir, ctx.Err())
		case <-ticker.C:
		}
	}
}

// giveBack kills a sandbox Create made and could not finish, so that a failed Create leaves
// nothing behind. ctx may be what ended the wait, so the kill runs on its own time.
func (p *Provider) giveBack(ctx context.Context, id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
	defer cancel()
	if err := p.Destroy(ctx, id); err != nil {
		return fmt.Errorf("%w; destroying it failed and it may still be billed: %v", cause, err)
	}
	return fmt.Errorf("%w; it was destroyed", cause)
}

func (p *Provider) awaitListed(ctx context.Context, id, name string) (sandbox, error) {
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		sandboxes, err := p.sandboxes(ctx, name)
		if err != nil && ctx.Err() == nil {
			return sandbox{}, fmt.Errorf("e2b: sandbox %s was created for %q and could not be confirmed in the list: %w", id, name, err)
		}
		for _, found := range sandboxes {
			if found.SandboxID == id && found.Metadata[nameKey] == name {
				return found, nil
			}
		}
		select {
		case <-ctx.Done():
			return sandbox{}, fmt.Errorf("e2b: sandbox %s was created for %q and is not in the list: %w", id, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateSpec(spec provider.Spec) error {
	if err := provider.ValidateName(spec.Name); err != nil {
		return fmt.Errorf("e2b: %w", err)
	}
	if spec.Image == "" {
		return fmt.Errorf("e2b: spec %q has no image", spec.Name)
	}
	if len(spec.Command) == 0 || spec.Command[0] == "" {
		return fmt.Errorf("e2b: spec %q has no command", spec.Name)
	}
	if spec.AcceleratorCount != 0 {
		return fmt.Errorf("e2b: spec %q asks for %d accelerators of type %q, and the vendor rents none", spec.Name, spec.AcceleratorCount, spec.AcceleratorType)
	}
	if spec.CPUMillis <= 0 || spec.MemoryBytes <= 0 || spec.DiskBytes <= 0 {
		return fmt.Errorf("e2b: spec %q must state cpu, memory and disk", spec.Name)
	}
	for name := range spec.Env {
		if !envName.MatchString(name) {
			return fmt.Errorf("e2b: spec %q: %q is not an environment variable name", spec.Name, name)
		}
	}
	return nil
}
