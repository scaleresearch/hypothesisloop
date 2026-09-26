package scheduler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// admitStore is the rows a force-admit reads and claims. ClaimSubmitted checks capacity against
// the rows SUBMITTED on the cluster and keeps the resolved job that would be written.
type admitStore struct {
	Store
	experiments map[string]*domain.Experiment
	written     map[string]domain.JobSpec
	cluster     *domain.Cluster
}

func (s *admitStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	exp := *s.experiments[id]
	return &exp, nil
}

func (s *admitStore) ClaimSubmitted(ctx context.Context, id, clusterName string, resolvedJob *domain.JobSpec, capacityAvailable func(context.Context, []*domain.Experiment) (bool, error)) (bool, error) {
	var desired []*domain.Experiment
	for _, exp := range s.experiments {
		if exp.Status == domain.StatusSubmitted && exp.ClusterName == clusterName {
			desired = append(desired, exp)
		}
	}
	ok, err := capacityAvailable(ctx, desired)
	if err != nil || !ok {
		return false, err
	}
	if s.written == nil {
		s.written = map[string]domain.JobSpec{}
	}
	s.written[id] = *resolvedJob
	return true, nil
}

// admitWorkload reports the slot fixture as the one connected cluster.
type admitWorkload struct {
	WorkloadClient
	c         *slotFixture
	multiNode bool
	created   []*domain.Experiment
}

func (w *admitWorkload) GetFlavorCapacity(context.Context) (map[string]domain.Footprint, map[string]domain.Footprint, error) {
	return map[string]domain.Footprint{rented: w.c.capacity()}, nil, nil
}
func (w *admitWorkload) GetAcceleratorCapacityByNode(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{rented: w.c.accelerators}, nil
}
func (w *admitWorkload) GetNodeResourceCapacity(context.Context) (map[string]map[string]map[string]int64, error) {
	return map[string]map[string]map[string]int64{rented: w.c.free}, nil
}
func (w *admitWorkload) GetNodeLabels(context.Context) (map[string]map[string]map[string]string, error) {
	return map[string]map[string]map[string]string{rented: w.c.labels}, nil
}
func (w *admitWorkload) GetMultiNodeCapability(context.Context) (map[string]bool, error) {
	return map[string]bool{rented: w.multiNode}, nil
}
func (w *admitWorkload) CreateWorkload(_ context.Context, exp *domain.Experiment) error {
	w.created = append(w.created, exp)
	return nil
}

func admitHandler(store *admitStore, workload *admitWorkload) *Handler {
	return NewHandler(admissionsUnrecorded(NewService(store, nil, workload, nil, nil, noopSettler{}, testGraceLimits, "http://metrics.invalid", zap.NewNop())).WithLoop(noopLoop{}))
}

func queued(exp *domain.Experiment) *domain.Experiment {
	exp.Status = domain.StatusQueued
	return exp
}

// A force-admit onto a cluster of slots admits the job the way the tick does: sized to a free
// slot and pinned to it, in the resolved job the runtime is handed.
func TestForceAdmitOntoSlotsSizesAndPinsTheJob(t *testing.T) {
	c := twoH100AndACPU()
	store := &admitStore{experiments: map[string]*domain.Experiment{"exp-1": queued(h100Job("exp-1", "2"))}}
	workload := &admitWorkload{c: c}

	out, err := admitHandler(store, workload).admit(context.Background(), "exp-1", rented)
	if err != nil {
		t.Fatal(err)
	}
	written := store.written["exp-1"]
	if slot := slotOf(written); (slot != "h100-0" && slot != "h100-1") || written.CPU != "8" || written.Memory != "32Gi" {
		t.Fatalf("written as %+v", written)
	}
	if written.TerminationGracePeriodSeconds == nil {
		t.Fatal("the resolved job lost its grace")
	}
	if len(workload.created) != 1 || slotOf(workload.created[0].EffectiveJob()) != slotOf(written) || out.Body.Job.CPU != "2" {
		t.Fatalf("created %+v, answered %+v", workload.created, out.Body.Job)
	}
}

// A formerly pinned job is admitted from the job it submitted. Planned from what an earlier
// admission resolved, it could go only to the slot it had held, which here is taken.
func TestForceAdmitDoesNotPlanFromAnEarlierAdmissionsPin(t *testing.T) {
	c := twoH100AndACPU()
	stale := queued(pinnedTo(sized(t, c, h100Job("exp-1", "2")), "h100-0"))
	holder := pinnedTo(sized(t, c, h100Job("holder", "2")), "h100-0")
	holder.Status, holder.ClusterName = domain.StatusSubmitted, rented
	c.take("h100-0")
	store := &admitStore{experiments: map[string]*domain.Experiment{"exp-1": stale, "holder": holder}}

	if _, err := admitHandler(store, &admitWorkload{c: c}).admit(context.Background(), "exp-1", rented); err != nil {
		t.Fatalf("admit with h100-1 free: %v", err)
	}
	if slot := slotOf(store.written["exp-1"]); slot != "h100-1" {
		t.Fatalf("admitted to %q, want the free slot", slot)
	}
}

func TestForceAdmitIsRefusedWhenNoSlotTakesTheJob(t *testing.T) {
	c := twoH100AndACPU().take("cpu-0")
	store := &admitStore{experiments: map[string]*domain.Experiment{
		// The only slot without accelerators is taken, and the free H100 slots are not its to hold.
		"cpu": queued(cpuJob("cpu", "2")),
		// No slot is this large.
		"large": queued(h100Job("large", "64")),
	}}
	for id := range store.experiments {
		_, err := admitHandler(store, &admitWorkload{c: c}).admit(context.Background(), id, rented)
		if err == nil || !strings.Contains(err.Error(), "insufficient capacity") {
			t.Errorf("%s: err = %v, written %+v", id, err, store.written[id])
		}
	}
}

// A job that spans nodes is admitted only to a cluster that reports it runs such jobs, by the
// tick and by hand alike. Admitted to one that does not, the runtime refused it every pass.
func TestForceAdmitOfAMultiNodeJobNeedsAClusterThatRunsThem(t *testing.T) {
	c := newSlots().add("node-a", 1, size(8000, 32, 100)).add("node-b", 1, size(8000, 32, 100))
	for node := range c.labels {
		delete(c.labels[node], domain.NodeSlotLabel)
	}
	twoNodes := queued(h100Job("exp-1", "2"))
	twoNodes.Job.NumNodes, twoNodes.AcceleratorCount = 2, 2
	store := &admitStore{experiments: map[string]*domain.Experiment{"exp-1": twoNodes}}

	workload := &admitWorkload{c: c}
	_, err := admitHandler(store, workload).admit(context.Background(), "exp-1", rented)
	if err == nil || !strings.Contains(err.Error(), "more than one node") || len(store.written) != 0 || len(workload.created) != 0 {
		t.Fatalf("on a cluster that runs one-node jobs: err = %v, written %v", err, store.written)
	}
	workload.multiNode = true
	if _, err := admitHandler(store, workload).admit(context.Background(), "exp-1", rented); err != nil {
		t.Fatalf("on a cluster that runs them: %v", err)
	}
}

// The org a job belongs to decides which clusters its jobs use. The administrator of a shared
// cluster placing the job by hand does not overrule it, and is told why.
func TestForceAdmitOntoAClusterTheJobsOrgTurnedOffIsRefused(t *testing.T) {
	exp := queued(h100Job("exp-1", "2"))
	exp.OrgID = "org-a"
	store := &admitStore{
		experiments: map[string]*domain.Experiment{"exp-1": exp},
		cluster:     &domain.Cluster{Name: rented, OrgID: "org-staff", Shared: true, DisabledOrgs: []string{"org-a"}},
	}
	workload := &admitWorkload{c: twoH100AndACPU()}

	_, err := admitHandler(store, workload).admit(context.Background(), "exp-1", rented)

	var refusal *refused
	if !errors.As(err, &refusal) || refusal.GetStatus() != http.StatusConflict || refusal.Reason != ReasonClusterDisabled {
		t.Fatalf("err = %v, want a 409 with reason %s", err, ReasonClusterDisabled)
	}
	if len(store.written) != 0 || len(workload.created) != 0 {
		t.Fatalf("the refused job was written %v and created %v", store.written, workload.created)
	}

	store.cluster.DisabledOrgs = []string{"org-b"}
	if _, err := admitHandler(store, workload).admit(context.Background(), "exp-1", rented); err != nil {
		t.Fatalf("on a shared cluster another org turned off: %v", err)
	}
}

// admissionsUnrecorded is a service whose admissions reach no metrics store.
func admissionsUnrecorded(s *Service) *Service {
	s.recordAdmission = func(context.Context, string, time.Time) error { return nil }
	return s
}
