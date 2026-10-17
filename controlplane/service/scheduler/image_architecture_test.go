package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler/placement"
)

func imagePE(image *domain.ExperimentImage) *domain.PlatformExperiment {
	return &domain.PlatformExperiment{ID: "pe-1", Image: image}
}

func admissionReason(t *testing.T, err error) *AdmissionError {
	t.Helper()
	var refused *AdmissionError
	if !errors.As(err, &refused) {
		t.Fatalf("got %v, want an AdmissionError", err)
	}
	return refused
}

// A job runs in the image its platform experiment records when it is submitted, and on nodes of
// that image's architecture, which its node selector says from then on beside what it stated.
func TestAJobTakesItsPlatformExperimentsImageAndArchitecture(t *testing.T) {
	exp := humanIdeaSubmission()
	stated := map[string]string{"zone": "b"}
	exp.Job.NodeSelector = stated
	if err := takeImage(imagePE(testExperimentImage), exp); err != nil {
		t.Fatal(err)
	}
	if exp.Image != testExperimentImage.Image {
		t.Errorf("image = %q, want %q", exp.Image, testExperimentImage.Image)
	}
	if got := exp.Job.NodeSelector; len(got) != 2 || got["zone"] != "b" || got[domain.NodeArchitectureLabel] != "amd64" {
		t.Errorf("node selector = %v, want zone b and %s amd64", got, domain.NodeArchitectureLabel)
	}
	if len(stated) != 1 {
		t.Errorf("the submitted selector was written to: %v", stated)
	}

	// One that states the image's own architecture is taken as it is.
	same := humanIdeaSubmission()
	same.Job.NodeSelector = map[string]string{domain.NodeArchitectureLabel: "amd64"}
	if err := takeImage(imagePE(testExperimentImage), same); err != nil || same.Job.NodeSelector[domain.NodeArchitectureLabel] != "amd64" {
		t.Fatalf("a selector stating the image's architecture: %v, %v", same.Job.NodeSelector, err)
	}
}

// A job that states another architecture than its image runs on could never start where it is
// placed, and is refused.
func TestAJobStatingAnotherArchitectureThanItsImageIsRefused(t *testing.T) {
	exp := humanIdeaSubmission()
	exp.Job.NodeSelector = map[string]string{domain.NodeArchitectureLabel: "arm64"}
	refused := admissionReason(t, takeImage(imagePE(testExperimentImage), exp))
	if refused.Reason != ReasonMalformed || refused.Field != "job.node_selector" || !strings.Contains(refused.Message, "arm64") || !strings.Contains(refused.Message, "amd64") {
		t.Fatalf("refused %+v, want job.node_selector naming both architectures", refused)
	}
	if exp.Image != "" {
		t.Errorf("a refused job took image %q", exp.Image)
	}
}

func TestAJobOfAPlatformExperimentWithoutAnImageIsRefused(t *testing.T) {
	refused := admissionReason(t, takeImage(imagePE(nil), humanIdeaSubmission()))
	if refused.Reason != domain.ReasonNoExperimentImage || !strings.Contains(refused.Message, "hl image set") {
		t.Fatalf("refused %+v, want %s naming hl image set", refused, domain.ReasonNoExperimentImage)
	}
	noCommand := humanIdeaSubmission()
	noCommand.Job.Command = nil
	if refused := admissionReason(t, takeImage(imagePE(testExperimentImage), noCommand)); refused.Reason != ReasonRequired || refused.Field != "job.command" {
		t.Fatalf("a job with no command: %+v", refused)
	}
}

// archWorkload is orgWorkload whose nodes carry the architecture label each runtime reports: a
// kubelet's own on k8s, the agent's GOARCH on bare metal.
type archWorkload struct {
	orgWorkload
	arch map[string]string
}

func (w archWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	out := map[string]map[string]map[string]string{}
	for name := range w.clusters {
		out[name] = map[string]map[string]string{"node-" + name: {domain.NodeArchitectureLabel: w.arch[name]}}
	}
	return out, nil
}

func archJob(id string, tier domain.CapacityTier, arch string) *domain.Experiment {
	exp := orgJob(id, "org-a", tier, 1)
	exp.Job.NodeSelector = map[string]string{domain.NodeArchitectureLabel: arch}
	return exp
}

// A job is placed only on a cluster whose nodes are of its image's architecture, and waits for
// one beside an idle cluster of another.
func TestTickNeverPlacesAJobOnANodeOfAnotherArchitecture(t *testing.T) {
	for _, tier := range []domain.CapacityTier{domain.CapacityGuaranteed, domain.CapacityBurst} {
		t.Run(string(tier), func(t *testing.T) {
			store := &orgStore{registered: map[string]domain.Cluster{
				"amd-box": {Name: "amd-box", OrgID: "org-a"},
				"arm-box": {Name: "arm-box", OrgID: "org-a"},
			}}
			store.queued = []*domain.Experiment{archJob("on-amd", tier, "amd64"), archJob("on-arm", tier, "arm64"), archJob("on-amd-2", tier, "amd64")}
			workload := archWorkload{
				orgWorkload: orgWorkload{clusters: map[string]orgCluster{"amd-box": {free: 1, installed: 1}, "arm-box": {free: 4, installed: 4}}},
				arch:        map[string]string{"amd-box": "amd64", "arm-box": "arm64"},
			}
			l := holdLoop(store, workload, noopEvictor{})
			l.settler = &recordingSettler{}

			tickOnce(t, l)

			if got := store.submitted["on-arm"]; got != "arm-box" {
				t.Errorf("the arm64 job was placed on %q, want arm-box", got)
			}
			amd := []string{store.submitted["on-amd"], store.submitted["on-amd-2"]}
			placed := 0
			for _, cluster := range amd {
				if cluster == "arm-box" {
					t.Fatalf("an amd64 job was placed on the arm64 cluster: %v", amd)
				}
				if cluster == "amd-box" {
					placed++
				}
			}
			if placed != 1 {
				t.Errorf("amd64 jobs placed on %v, want exactly one on amd-box's one free accelerator", amd)
			}
		})
	}
}

// A runtime that rents machines labels every slot amd64, the only architecture it rents: a job
// of another is sized to none of them and placed on none.
func TestARentedSlotTakesOnlyAnAmd64Job(t *testing.T) {
	c := twoH100AndACPU()
	for node := range c.labels {
		c.labels[node][domain.NodeArchitectureLabel] = "amd64"
	}
	amd, arm := h100Job("amd", "2"), h100Job("arm", "2")
	amd.Job.NodeSelector = map[string]string{domain.NodeArchitectureLabel: "amd64"}
	arm.Job.NodeSelector = map[string]string{domain.NodeArchitectureLabel: "arm64"}

	if _, misfit := placement.ResolveOntoSlot(amd, rented, c.sizes, c.free, c.labels); misfit != "" {
		t.Fatalf("an amd64 job: %s", misfit)
	}
	if resolved, misfit := placement.ResolveOntoSlot(arm, rented, c.sizes, c.free, c.labels); misfit == "" {
		t.Fatalf("an arm64 job was sized to a slot: %+v", resolved)
	}
	if node, _, misfit := placement.SlotFor(arm, rented, c.sizes, c.free, c.labels); node != "" || misfit == "" {
		t.Fatalf("an arm64 job was given slot %q", node)
	}
}
