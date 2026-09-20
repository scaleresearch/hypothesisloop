package vast

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// stopMark is appended to an instance's label to ask its supervisor to end the command. The
// vendor's own stop is no use for that: it kills the command about a second after SIGTERM. A
// name never holds a dot, so the mark cannot be part of one, and List takes it off again.
const stopMark = ".stop"

// Stop marks the instance. Its supervisor reads its own label every few seconds, sends the
// command SIGTERM once when it finds the mark, and stops the instance after the command has
// ended in its own time.
func (p *Provider) Stop(ctx context.Context, id string) error {
	path := "/api/v0/instances/" + url.PathEscape(id) + "/"
	var shown struct {
		Instance *instance `json:"instances"`
	}
	if _, err := p.call(ctx, http.MethodGet, path+"?owner=me", nil, &shown); err != nil {
		return err
	}
	if shown.Instance == nil {
		return nil
	}
	if shown.Instance.Label == nil {
		return fmt.Errorf("vast: instance %s has no label to mark", id)
	}
	if strings.HasSuffix(*shown.Instance.Label, stopMark) {
		return nil
	}
	var marked struct {
		Success bool   `json:"success"`
		Message string `json:"msg"`
	}
	_, err := p.call(ctx, http.MethodPut, path, map[string]string{"label": *shown.Instance.Label + stopMark}, &marked)
	if isGone(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !marked.Success {
		return fmt.Errorf("vast: marking instance %s to stop was refused: %s", id, marked.Message)
	}
	return nil
}
