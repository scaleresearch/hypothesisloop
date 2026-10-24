package clusterexchange

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// Agents report every accelerator type their hardware publishes; capacity keeps only the priced
// ones, matched exactly.
func TestReconcileIngestKeepsOnlyPricedAcceleratorTypes(t *testing.T) {
	h := NewService(Config{PricedAcceleratorTypes: []string{"nvidia.com/gpu.product=Tesla-T4"}, MaxLogLineChars: 10, ProxyURL: "https://api.test/git"})
	report := clusterapi.ReconcileRequest{
		AcceleratorAvailableByType: map[string]int64{"nvidia.com/gpu.product=Tesla-T4": 1, "nvidia.com/gpu.product=tesla-t4": 1, "nvidia.com/cuda.driver.major=550": 1},
		AcceleratorTotalByType:     map[string]int64{"nvidia.com/gpu.product=Tesla-T4": 2, "nvidia.com/gpu.product=tesla-t4": 2, "nvidia.com/cuda.driver.major=550": 2},
		AcceleratorAvailableByNode: map[string]map[string]int64{
			"node-a": {"nvidia.com/gpu.product=Tesla-T4": 1, "nvidia.com/cuda.driver.major=550": 1},
		},
	}
	h.keepPricedAccelerators(&report)
	want := map[string]int64{"nvidia.com/gpu.product=Tesla-T4": 1}
	if !reflect.DeepEqual(report.AcceleratorAvailableByType, want) ||
		!reflect.DeepEqual(report.AcceleratorTotalByType, map[string]int64{"nvidia.com/gpu.product=Tesla-T4": 2}) ||
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

type contactStore struct {
	Store
	refusal string
}

func (s *contactStore) RecordContactRefusal(_ context.Context, _, refusal string) error {
	s.refusal = refusal
	return nil
}

// A report naming a slot whose count label is no count is refused whole, before anything it
// reports is recorded, and the refusal is kept as the cluster's last error.
func TestReconcileRefusesAReportWithAMalformedSlotLabel(t *testing.T) {
	store := &contactStore{}
	h := &Service{store: store, logger: zap.NewNop()}
	report := clusterapi.ReconcileRequest{
		AcceleratorAvailableByType: map[string]int64{}, AcceleratorTotalByType: map[string]int64{},
		AcceleratorAvailableByNode: map[string]map[string]int64{},
		NodeResourcesByNode:        map[string]map[string]int64{}, NodeResourcesTotalByNode: map[string]map[string]int64{},
		NodeLabelsByNode: map[string]map[string]string{"slot-1": {domain.NodeSlotLabel: "slot-1", domain.NodeSlotNetworkLabel: "two"}},
	}
	_, err := h.Reconcile(context.Background(), "edge-7", "v1", report)
	if err == nil || !strings.Contains(err.Error(), "slot-1") || !strings.Contains(err.Error(), domain.NodeSlotNetworkLabel) {
		t.Fatalf("reconcile = %v, want a refusal naming the node and its label", err)
	}
	if store.refusal == "" {
		t.Error("the refusal is not kept as the cluster's last error")
	}
}
