package fly

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider/vendorapi"
)

var _ provider.Warmer = (*Provider)(nil)

// warmKey is the metadata key that marks a Machine Warm created and no StartWarm has given a
// spec. StartWarm replaces the whole metadata, which drops it.
const warmKey = "hl_warm"

// idle is what a warm Machine runs should anything start it before StartWarm does: nothing.
var idle = []string{"/bin/sh", "-c", "exit 0"}

// replaceWaitSeconds bounds the wait for an update to finish replacing a Machine, inside the
// client's own timeout.
const replaceWaitSeconds = 25

// Warm creates the Machine with skip_launch. What real Machines taught:
//
//   - The vendor places it, answers "created" at once and pulls the image in the background; the
//     Machine is "stopped" once the image is on its host, which is StateWarm here. A 0.9 GB
//     image took 15 seconds.
//   - A stopped Machine is billed for its root filesystem only.
//   - An update of a stopped Machine leaves it stopped and replaces it in about two seconds when
//     the image is already on the host, and a start before the replacement is done is refused
//     with 412. A start after it had the command running in under two seconds.
func (p *Provider) Warm(ctx context.Context, spec provider.Spec) (string, error) {
	spec.Command, spec.Env = idle, nil
	req, err := p.newCreateRequest(spec)
	if err != nil {
		return "", err
	}
	req.SkipLaunch = true
	req.Config.Init.Exec = idle
	req.Config.Metadata[warmKey] = "1"
	var created struct {
		ID string `json:"id"`
	}
	if _, err := p.call(ctx, http.MethodPost, p.machinesPath(""), req, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", fmt.Errorf("fly: warming a machine for %q answered without an id", spec.Name)
	}
	if err := p.awaitListed(ctx, created.ID, spec.Name); err != nil {
		destroy := func(ctx context.Context) error { return p.Destroy(ctx, created.ID) }
		return "", vendorapi.GiveBack(ctx, destroy, requestTimeout, err)
	}
	return created.ID, nil
}

// StartWarm updates the Machine to the config Create would have made for spec, waits for the
// vendor to finish replacing it and starts it.
func (p *Provider) StartWarm(ctx context.Context, id string, spec provider.Spec) error {
	req, err := p.newCreateRequest(spec)
	if err != nil {
		return err
	}
	req.SkipLaunch = true
	path := p.machinesPath("/" + url.PathEscape(id))
	var updated struct {
		InstanceID string `json:"instance_id"`
	}
	if _, err := p.call(ctx, http.MethodPost, path, req, &updated); err != nil {
		return err
	}
	if updated.InstanceID == "" {
		return fmt.Errorf("fly: updating machine %s for %q answered without an instance id", id, spec.Name)
	}
	wait := fmt.Sprintf("%s/wait?state=stopped&timeout=%d&instance_id=%s", path, replaceWaitSeconds, url.QueryEscape(updated.InstanceID))
	if _, err := p.call(ctx, http.MethodGet, wait, nil, nil); err != nil {
		return fmt.Errorf("fly: machine %s was updated for %q and did not settle: %w", id, spec.Name, err)
	}
	if _, err := p.call(ctx, http.MethodPost, path+"/start", nil, nil); err != nil {
		return err
	}
	return p.awaitListed(ctx, id, spec.Name)
}
