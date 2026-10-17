package clusterexchange

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Agents report every accelerator type their hardware publishes; capacity keeps only the priced
// ones, matched case-insensitively.
func TestReconcileIngestKeepsOnlyPricedAcceleratorTypes(t *testing.T) {
	h := NewService(nil, 0, nil, 0, nil, 0, []string{"nvidia.com/gpu.product=Tesla-T4"}, 10, "", "https://api.test/git", nil, nil)
	report := clusterapi.ReconcileRequest{
		AcceleratorAvailableByType: map[string]int64{"nvidia.com/gpu.product=tesla-t4": 1, "nvidia.com/cuda.driver.major=550": 1},
		AcceleratorTotalByType:     map[string]int64{"nvidia.com/gpu.product=tesla-t4": 2, "nvidia.com/cuda.driver.major=550": 2},
		AcceleratorAvailableByNode: map[string]map[string]int64{
			"node-a": {"nvidia.com/gpu.product=tesla-t4": 1, "nvidia.com/cuda.driver.major=550": 1},
		},
	}
	h.keepPricedAccelerators(&report)
	want := map[string]int64{"nvidia.com/gpu.product=tesla-t4": 1}
	if !reflect.DeepEqual(report.AcceleratorAvailableByType, want) ||
		!reflect.DeepEqual(report.AcceleratorTotalByType, map[string]int64{"nvidia.com/gpu.product=tesla-t4": 2}) ||
		!reflect.DeepEqual(report.AcceleratorAvailableByNode["node-a"], want) {
		t.Fatalf("filtered report = %+v", report)
	}
}

func TestStatusIngestSplitsLongLogLinesWithoutDroppingAnything(t *testing.T) {
	got := splitLongLines([]string{"short", strings.Repeat("x", 25)}, 10)
	want := []string{"short", strings.Repeat("x", 10), strings.Repeat("x", 10), strings.Repeat("x", 5)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("split = %q, want %q", got, want)
	}
}

// Each job in a reconcile answer carries its own token, the same one on every pass, and one that
// names that job only.
func TestReconcileHandsEachJobItsOwnToken(t *testing.T) {
	const secret = "test-signing-secret-of-enough-length"
	h := wireHandler(t)
	h.signingSecret = secret
	tokens := map[string]string{}
	for _, id := range []string{"job-1", "job-2", "job-1"} {
		job, err := h.desiredJob(context.Background(), &domain.Experiment{Image: wireImage, ID: id, PlatformExperimentID: "pe-1"})
		if err != nil {
			t.Fatalf("desired job: %v", err)
		}
		if job.GitURL != "https://api.test/git/pe-1.git" {
			t.Errorf("repository of %s: got = %q, want its platform experiment's through the proxy", id, job.GitURL)
		}
		want, _ := auth.Mint(secret, scope.Job(id))
		if job.Token == "" || job.Token != want {
			t.Errorf("token of %s: got = %q, want the job-scoped token %q", id, job.Token, want)
		}
		tokens[id] = job.Token
	}
	if tokens["job-1"] == tokens["job-2"] {
		t.Error("two jobs were handed the same token")
	}
	h.signingSecret = ""
	if _, err := h.desiredJob(context.Background(), &domain.Experiment{Image: wireImage, ID: "job-1"}); err == nil {
		t.Error("desired job with no signing secret: got = nil, want an error")
	}
}
