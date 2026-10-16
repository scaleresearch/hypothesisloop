package scheduler

import (
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A chip job died opening its device with "no huge page mount found": hugepages were something
// each job spec had to know to ask for. What a type always needs is the catalog's, and admission
// writes it into the job the runtime runs, whatever the job stated.
func TestAdmissionAddsWhatTheCatalogSaysAJobOnTheTypeAlwaysNeeds(t *testing.T) {
	withCPUType(t)

	plain := priced(holdJob("plain", domain.CapacityBurst, h100, 1, "1"), 1)
	stating := priced(holdJob("stating", domain.CapacityBurst, h100, 1, "1"), 1)
	stating.AgentID = "agent-2"
	stating.Job.AcceleratorPodResources = map[string]string{"hugepages-1Gi": "1Gi", "hugepages-2Mi": "64Mi"}
	cores := priced(coresJob("cores", domain.CapacityBurst, 2), 1)
	store := &mixedStore{holdStore: holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{plain, stating, cores}}}}
	l, _, _ := mixedLoop(store, mixedWorkload{h100Free: 4, cpuFree: 16, published: true}, nil)
	l.WithPodResources(domain.NewPodResources(map[string]map[string]string{h100: {"hugepages-1Gi": "4Gi"}}))

	tickOnce(t, l)

	for id, want := range map[string]map[string]string{
		"plain":   {"hugepages-1Gi": "4Gi"},
		"stating": {"hugepages-1Gi": "4Gi", "hugepages-2Mi": "64Mi"},
		"cores":   nil,
	} {
		resolved, admitted := store.resolved[id]
		if !admitted {
			t.Fatalf("%s was not admitted: %q", id, store.notAdmitted[id])
		}
		if !reflect.DeepEqual(resolved.AcceleratorPodResources, want) {
			t.Errorf("%s runs with accelerator_pod_resources %v, want %v", id, resolved.AcceleratorPodResources, want)
		}
	}
	if plain.Job.AcceleratorPodResources != nil || stating.Job.AcceleratorPodResources["hugepages-1Gi"] != "1Gi" {
		t.Errorf("the submitted jobs were rewritten: %v, %v", plain.Job.AcceleratorPodResources, stating.Job.AcceleratorPodResources)
	}
}
