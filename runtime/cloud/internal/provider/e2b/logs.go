package e2b

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Logs runs tail on the output file inside the sandbox, which needs tail in the template. A
// sandbox that is gone has no output left to read: the vendor keeps nothing of a killed sandbox.
func (p *Provider) Logs(ctx context.Context, id string, tail int) ([]string, error) {
	if tail <= 0 {
		return nil, fmt.Errorf("e2b: logs of sandbox %s: tail must be positive, got %d", id, tail)
	}
	inside, exists, err := p.inside(ctx, id)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("e2b: logs of sandbox %s: the sandbox is gone, and its output with it", id)
	}
	var stdout, stderr strings.Builder
	status := ""
	read := startRequest{Process: processConfig{Cmd: "/bin/sh", Args: []string{"-c", tailScript, "hl-logs", outputPath, strconv.Itoa(tail)}}}
	err = inside.start(ctx, read, func(event processEvent) bool {
		if data := event.Event.Data; data != nil {
			stdout.Write(data.Stdout)
			stderr.Write(data.Stderr)
		}
		if end := event.Event.End; end != nil {
			status = end.Status
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("e2b: logs of sandbox %s: %w", id, err)
	}
	if status != "exit status 0" {
		return nil, fmt.Errorf("e2b: logs of sandbox %s: reading the output file ended with %q: %s", id, status, truncate([]byte(stderr.String())))
	}
	if stdout.Len() == 0 {
		return []string{}, nil
	}
	return strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n"), nil
}
