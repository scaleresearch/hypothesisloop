package fly

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

var _ provider.Extender = (*Provider)(nil)

const (
	// execTimeoutSeconds bounds the command Extend runs in the Machine, inside the client's own
	// timeout.
	execTimeoutSeconds = 20
	// noEndKept is the exit code of a Machine whose supervisor keeps no end at endsAtPath: one
	// rented with no lifetime, or started before supervisors kept it there.
	noEndKept = 3
)

// Extend writes until where the Machine's supervisor reads its end from (supervisor), with a
// command the Machines API runs in the Machine. The supervisor reads it within a minute. The
// write replaces the file whole, so the supervisor never reads half of it.
func (p *Provider) Extend(ctx context.Context, id string, until time.Time) error {
	quoted := "'" + endsAtPath + "'"
	script := fmt.Sprintf("[ -f %[1]s ] || exit %[2]d; echo %[3]d > %[1]s.new && mv -f %[1]s.new %[1]s", quoted, noEndKept, until.Unix())
	var ran struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	body := map[string]any{"command": []string{"/bin/sh", "-c", script}, "timeout": execTimeoutSeconds}
	if _, err := p.call(ctx, http.MethodPost, p.machinesPath("/"+url.PathEscape(id)+"/exec"), body, &ran); err != nil {
		return fmt.Errorf("fly: move the end of machine %s to %s: %w", id, until.UTC().Format(time.RFC3339), err)
	}
	switch ran.ExitCode {
	case 0:
		return nil
	case noEndKept:
		return fmt.Errorf("fly: machine %s keeps no end at %s: it was rented with no lifetime or started by an older supervisor, and keeps the end it has", id, endsAtPath)
	}
	return fmt.Errorf("fly: moving the end of machine %s exited %d: %s", id, ran.ExitCode, ran.Stderr)
}
