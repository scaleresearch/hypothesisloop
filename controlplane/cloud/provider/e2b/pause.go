package e2b

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
)

var _ provider.Pauser = (*Provider)(nil)

// Pause has the vendor snapshot the sandbox, memory and filesystem, and stop running it. Resume
// continues the same processes: the supervisor and the command go on where they were.
//
// The vendor keeps a paused sandbox until it is destroyed.
//
// The API answers 404 for a sandbox that is gone and 409 for one that is paused already. Neither
// is an error here.
func (p *Provider) Pause(ctx context.Context, id string) error {
	_, err := p.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/pause", nil, nil)
	if hasStatus(err, http.StatusNotFound) || hasStatus(err, http.StatusConflict) {
		return nil
	}
	return err
}

// Resume connects to the sandbox, which resumes a paused one and leaves a running one running.
// It asks for a whole lifetime from now, and the vendor only ever extends what a sandbox has
// left. Whether a plan's cap on a lifetime counts across a pause is not known from a test.
func (p *Provider) Resume(ctx context.Context, id string) error {
	_, err := p.call(ctx, http.MethodPost, "/v2/sandboxes/"+url.PathEscape(id)+"/connect", map[string]int{"timeout": p.cfg.LifetimeSeconds}, nil)
	if hasStatus(err, http.StatusNotFound) {
		return fmt.Errorf("e2b: sandbox %s is gone and cannot be resumed: %w", id, err)
	}
	return err
}
