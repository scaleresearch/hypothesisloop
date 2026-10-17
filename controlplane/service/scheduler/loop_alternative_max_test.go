package scheduler

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// smallFirstTypeWorkload is cluster-a with one node of two h100 and eight l40, all free.
type smallFirstTypeWorkload struct{ holdWorkload }

func (smallFirstTypeWorkload) acceleratorTypes() map[string]int64 {
	return map[string]int64{h100: 2, l40: 8}
}
func (w smallFirstTypeWorkload) GetAcceleratorTypeCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	free := func() map[string]domain.Footprint {
		return map[string]domain.Footprint{"cluster-a": domain.CapacityFootprint(16, w.acceleratorTypes(), 64<<30, 64<<30)}
	}
	return free(), free(), nil
}
func (w smallFirstTypeWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": w.acceleratorTypes()}}, nil
}
func (smallFirstTypeWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": holdNodeTotal()}}, nil
}
func (smallFirstTypeWorkload) GetNodeTotalCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{"cluster-a": {"node-a": holdNodeTotal()}}, nil
}
func (smallFirstTypeWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{"cluster-a": {"node-a": {}}}, nil
}
func (w smallFirstTypeWorkload) GetTotalCapacity(context.Context) (map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{"cluster-a": domain.CapacityFootprint(16, w.acceleratorTypes(), 64<<30, 64<<30)}, nil
}

// A job asks for four of a type the cluster has two of and accepts a type it has eight of. It is
// placed at the alternative and held to its share there: 4 of 8 devices, half the node. The share
// at the first type, which would be twice the node, decides nothing.
func TestAJobTooLargeForItsFirstTypeTakesAnAlternativeThatFits(t *testing.T) {
	withCPUType(t)
	for memory, resolved := range map[string]string{"1Gi": "1Gi", "32Gi": "32Gi"} {
		for _, tier := range []domain.CapacityTier{domain.CapacityBurst, domain.CapacityGuaranteed} {
			job := priced(holdJob("job", tier, h100, 4, "1"), 1)
			job.Job.Memory = memory
			job.Job.AcceptableAcceleratorTypes = []domain.AcceleratorType{l40}
			store := &mixedStore{holdStore: holdStore{noRunningListStore: noRunningListStore{t: t, queued: []*domain.Experiment{job}}}}
			l, quota, _ := mixedLoop(store, smallFirstTypeWorkload{}, nil)

			tickOnce(t, l)

			if store.submitted["job"] != "cluster-a" || job.AcceleratorType != l40 {
				t.Fatalf("memory %s, %s: placed on %q as %q (reason %q), want the alternative", memory, tier, store.submitted["job"], job.AcceleratorType, store.notAdmitted["job"])
			}
			if got := store.resolved["job"].Memory; got != resolved {
				t.Errorf("memory %s, %s: runs with %q, want %q", memory, tier, got, resolved)
			}
			if got := quota.reserved["job"]; got.AcceleratorType != l40 || !near(got.Cost, 4*0.25) {
				t.Errorf("memory %s, %s: reserved at %+v, want four l40 for an hour", memory, tier, got)
			}
		}
	}
}
