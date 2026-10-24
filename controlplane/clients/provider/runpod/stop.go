package runpod

import (
	"context"
	"net/http"
	"net/url"
)

// Stop marks the pod's name with stopMark. Its supervisor reads its own name every few seconds,
// sends the command SIGTERM once when it finds the mark, and keeps the pod after the command has
// ended in its own time. A pod whose command has ended is not marked.
func (p *Provider) Stop(ctx context.Context, id string) error {
	path := "/v2/pods/" + url.PathEscape(id)
	var found pod
	err := p.call(ctx, http.MethodGet, path, nil, &found)
	if status(err) == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	name := parseName(found.Name)
	if found.Status == statusTerminated || name.stopped || name.up == exitMark {
		return nil
	}
	name.stopped = true
	err = p.call(ctx, http.MethodPatch, path, map[string]string{"name": name.String()}, nil)
	if status(err) == http.StatusNotFound {
		return nil
	}
	return err
}
