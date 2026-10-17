package shared

import (
	"bytes"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/clitest"
)

func TestCapacityWarnsOfAJobImageNoOtherMachinePulls(t *testing.T) {
	clitest.RecordingServer(t, func(int, clitest.Recorded) any {
		return map[string]any{"clusters": []any{map[string]any{"cluster_name": "tt-box"}}, "job_images": []any{
			map[string]any{"name": "python", "image": "host.containers.internal:5000/hypothesisloop-job:t1"},
			map[string]any{"name": "remote", "image": "100.64.0.5:5000/hypothesisloop-job:t1"},
		}}
	})
	var stdout, stderr bytes.Buffer
	if code := clitest.Run(Capacity, []string{}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "job_image python is in a registry only the platform's own machine reaches (host.containers.internal:5000/hypothesisloop-job:t1)") {
		t.Errorf("stderr = %q, want python warned about", stderr.String())
	}
	if strings.Contains(stderr.String(), "job_image remote") {
		t.Errorf("stderr = %q, want no warning for an image other machines pull", stderr.String())
	}
}
