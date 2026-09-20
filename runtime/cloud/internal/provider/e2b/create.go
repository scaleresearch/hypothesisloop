package e2b

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider/vendorapi"
)

// nameKey is the metadata key a sandbox carries its instance name under.
const nameKey = "hl_name"

type createRequest struct {
	TemplateID string `json:"templateID"`
	// Timeout is the sandbox's whole lifetime in seconds. With AutoPause false the vendor kills
	// the sandbox when it ends.
	Timeout   int               `json:"timeout"`
	AutoPause bool              `json:"autoPause"`
	Metadata  map[string]string `json:"metadata"`
	Network   network           `json:"network"`
}

type network struct {
	// AllowPublicTraffic false keeps what the sandbox listens on from answering at its public
	// address without the sandbox's traffic token, which this adapter never asks for. The
	// vendor's default is to answer anyone.
	AllowPublicTraffic bool `json:"allowPublicTraffic"`
}

// Create starts a sandbox from the spec's template and then the command inside it. A sandbox
// that is smaller than the spec is killed before anything is started in it, and so is one whose
// command could not be started.
func (p *Provider) Create(ctx context.Context, spec provider.Spec) (string, error) {
	if err := validateSpec(spec); err != nil {
		return "", err
	}
	asked := sizeOf(spec)
	template := templateName(spec.Image, asked)
	var created struct {
		SandboxID       string `json:"sandboxID"`
		EnvdAccessToken string `json:"envdAccessToken"`
	}
	req := createRequest{TemplateID: template, Timeout: p.cfg.LifetimeSeconds, Metadata: map[string]string{nameKey: spec.Name}}
	_, err := p.call(ctx, http.MethodPost, "/v2/sandboxes", req, &created)
	if hasStatus(err, http.StatusNotFound) {
		return "", fmt.Errorf("e2b: template %s for image %q with %d cores and %d MiB of memory is not built; build it with `%s %s %d %d`: %w",
			template, spec.Image, asked.CPUs, asked.MemoryMB, templateScript, spec.Image, asked.CPUs, asked.MemoryMB, err)
	}
	if err != nil {
		return "", err
	}
	if created.SandboxID == "" || created.EnvdAccessToken == "" {
		return "", fmt.Errorf("e2b: creating a sandbox for %q answered without an id or without its token", spec.Name)
	}
	if err := p.launch(ctx, created.SandboxID, created.EnvdAccessToken, spec, template); err != nil {
		destroy := func(ctx context.Context) error { return p.Destroy(ctx, created.SandboxID) }
		return "", vendorapi.GiveBack(ctx, destroy, requestTimeout, err)
	}
	return created.SandboxID, nil
}

// launch sees a fresh sandbox in List, which is what tells a caller with no memory of this call
// that the name is taken, and compares what the list says of its size with the spec. Only then
// does anything of the spec enter the sandbox: the launch file with the command and its
// environment, into a directory no other user can read, and the start.
func (p *Provider) launch(ctx context.Context, id, token string, spec provider.Spec, template string) error {
	ctx, cancel := context.WithTimeout(ctx, p.window)
	defer cancel()
	listed, err := p.awaitListed(ctx, id, spec.Name)
	if err != nil {
		return err
	}
	asked := sizeOf(spec)
	if disk := provider.CeilDiv(spec.DiskBytes, bytesPerMB); listed.CPUCount < asked.CPUs || listed.MemoryMB < asked.MemoryMB || listed.DiskSizeMB < disk {
		return fmt.Errorf("e2b: template %s gives a sandbox %d cores, %d MiB of memory and %d MiB of disk, and spec %q asks for %d, %d and %d; nothing was started in it",
			template, listed.CPUCount, listed.MemoryMB, listed.DiskSizeMB, spec.Name, asked.CPUs, asked.MemoryMB, disk)
	}
	inside := envd{p: p, base: p.envdURL(id), token: token}
	if _, err := inside.run(ctx, maxUploadAnswer, "mkdir", "-m", "700", superviseDir); err != nil {
		return fmt.Errorf("e2b: sandbox %s was created for %q and the supervisor's directory could not be made: %w", id, spec.Name, err)
	}
	if err := inside.writeFile(ctx, launchPath, launchFile(spec.Command, spec.Env)); err != nil {
		return fmt.Errorf("e2b: sandbox %s was created for %q and its command could not be uploaded: %w", id, spec.Name, err)
	}
	start := startRequest{Process: processConfig{Cmd: "/bin/sh", Args: []string{launchPath}}, Tag: supervisorTag}
	if err := inside.start(ctx, start, func(processEvent) bool { return false }); err != nil {
		return fmt.Errorf("e2b: sandbox %s was created for %q and its command could not be started: %w", id, spec.Name, err)
	}
	return p.awaitSupervisor(ctx, inside, id, spec.Name, template)
}

// awaitSupervisor waits for the state file, the first thing the supervisor writes. envd reports
// a process as started before the shell has read a line, so only the file shows that the launch
// file ran as far as the supervisor.
func (p *Provider) awaitSupervisor(ctx context.Context, inside envd, id, name, template string) error {
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	for {
		_, written, err := inside.readFile(ctx, statePath, maxStateBytes)
		if err != nil && ctx.Err() == nil {
			return fmt.Errorf("e2b: sandbox %s was created for %q and its supervisor could not be seen starting: %w", id, name, err)
		}
		if written {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("e2b: sandbox %s was created for %q and its supervisor did not start; it needs sh and %s as %s writes it into template %s: %w",
				id, name, imageEnvPath, templateScript, template, ctx.Err())
		case <-ticker.C:
		}
	}
}

// awaitListed waits for the sandbox in the list. A list that fails is read again, since one
// refused call says nothing about the sandbox, and the sandbox is given up only when the window
// ends without a sighting.
func (p *Provider) awaitListed(ctx context.Context, id, name string) (sandbox, error) {
	ticker := time.NewTicker(p.poll)
	defer ticker.Stop()
	var failed error
	for {
		sandboxes, err := p.sandboxes(ctx, name)
		for _, found := range sandboxes {
			if found.SandboxID == id && found.Metadata[nameKey] == name {
				return found, nil
			}
		}
		if ctx.Err() == nil {
			failed = err
		}
		select {
		case <-ctx.Done():
			if failed != nil {
				return sandbox{}, fmt.Errorf("e2b: sandbox %s was created for %q and could not be confirmed in the list: %w", id, name, failed)
			}
			return sandbox{}, fmt.Errorf("e2b: sandbox %s was created for %q and is not in the list: %w", id, name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateSpec(spec provider.Spec) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("e2b: %w", err)
	}
	if spec.AcceleratorCount != 0 {
		return fmt.Errorf("e2b: spec %q asks for %d accelerators of type %q, and the vendor rents none", spec.Name, spec.AcceleratorCount, spec.AcceleratorType)
	}
	return nil
}
