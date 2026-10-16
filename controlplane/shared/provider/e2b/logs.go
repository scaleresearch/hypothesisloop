package e2b

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// maxLogBytes bounds what one Logs call reads. The command writes the lines, and tail counts
// lines whatever their length.
const maxLogBytes = 4 << 20

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
	output, err := inside.run(ctx, maxLogBytes, "/bin/sh", "-c", tailScript, "hl-logs", outputPath, strconv.Itoa(tail))
	if err != nil {
		return nil, fmt.Errorf("e2b: logs of sandbox %s, its last %d lines: %w", id, tail, err)
	}
	if output == "" {
		return []string{}, nil
	}
	return strings.Split(strings.TrimSuffix(output, "\n"), "\n"), nil
}
