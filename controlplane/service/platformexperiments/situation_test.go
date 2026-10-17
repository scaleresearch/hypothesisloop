package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

const typeA = domain.AcceleratorType("example.com/gpu=a")

// A job is placed whole on one cluster, each of its nodes on a node with room for it, and every
// job placed takes what it holds before the next is counted. Another type's devices place none.
func TestFitsNowCountsJobsShapedLikeTheBaselineNodeByNode(t *testing.T) {
	p := &Placement{
		Clusters: []ClusterCapacity{{ClusterName: "c1"}, {ClusterName: "c2"}},
		NodeAccelerators: map[string]map[string]map[string]int64{
			"c1": {"n1": {string(typeA): 4, "example.com/gpu=b": 8}, "n2": {string(typeA): 2}},
			"c2": {"n3": {"example.com/gpu=b": 8}},
		},
		NodeResources: map[string]map[string]map[string]int64{
			"c1": {"n1": {domain.NodeResourceCPUMillicores: 32000}, "n2": {domain.NodeResourceCPUMillicores: 4000}},
			"c2": {"n3": {domain.NodeResourceCPUMillicores: 64000}},
		},
	}
	shape := domain.NodeShape{AcceleratorCount: 2, Resources: map[string]int64{domain.NodeResourceCPUMillicores: 8000}}
	if got := fitsNow([]domain.NodeShape{shape}, typeA, p); got == nil || *got != 2 {
		t.Fatalf("one-node jobs: fits %v, want 2: n1 holds two, n2 has too few cores", got)
	}
	small := domain.NodeShape{AcceleratorCount: 2, Resources: map[string]int64{domain.NodeResourceCPUMillicores: 2000}}
	if got := fitsNow([]domain.NodeShape{small}, typeA, p); got == nil || *got != 3 {
		t.Fatalf("smaller one-node jobs: fits %v, want 3", got)
	}
	if got := fitsNow([]domain.NodeShape{small, small}, typeA, p); got == nil || *got != 1 {
		t.Fatalf("two-node jobs: fits %v, want 1 of the three nodes' worth", got)
	}
	if got := fitsNow([]domain.NodeShape{{}}, typeA, p); got != nil {
		t.Fatalf("a job that asks for nothing: fits %v, want no count", *got)
	}
	if got := fitsNow([]domain.NodeShape{shape}, typeA, &Placement{}); got == nil || *got != 0 {
		t.Fatalf("no cluster: fits %v, want 0", got)
	}
}

// The objective reads the best of the researchers' completed jobs and where it stood an hour
// and three hours before, the gap to the target in percent of it, short being positive, and the
// target a metric's target_vs_baseline sets against the baseline's value in either direction.
func TestTheObjectiveReadsTheBestAndWhereItStoodBefore(t *testing.T) {
	now := time.Now()
	values := []standing{
		{agentID: "a", jobID: "j1", value: 0.5, completedAt: now.Add(-4 * time.Hour)},
		{agentID: "b", jobID: "j2", value: 0.7, completedAt: now.Add(-2 * time.Hour)},
		{agentID: "a", jobID: "j3", value: 0.9, completedAt: now.Add(-10 * time.Minute)},
	}
	got := metricStanding(domain.MetricDefinition{Key: "acc", Direction: "maximize"}, values, now)
	if *got.BestValue != 0.9 || got.BestJob != "j3" || got.BestAgent != "a" || *got.Best1hAgo != 0.7 || *got.Best3hAgo != 0.5 {
		t.Fatalf("maximize: %+v", got)
	}
	got = metricStanding(domain.MetricDefinition{Key: "loss", Direction: "minimize"}, values, now)
	if *got.BestValue != 0.5 || got.BestJob != "j1" || *got.Best1hAgo != 0.5 {
		t.Fatalf("minimize: %+v", got)
	}
	if got := metricStanding(domain.MetricDefinition{Key: "acc", Direction: "maximize"}, nil, now); got.BestValue != nil || got.Best1hAgo != nil {
		t.Fatalf("no value: %+v", got)
	}
	f := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		direction    string
		best, target *float64
		want         *float64
	}{
		{"maximize", f(0.9), f(1.0), f(10)},
		{"maximize", f(1.1), f(1.0), f(-10)},
		{"minimize", f(2.5), f(2.0), f(25)},
		{"minimize", f(1.5), f(2.0), f(-25)},
		{"maximize", f(0.9), f(0), nil},
		{"maximize", nil, f(1), nil},
		{"maximize", f(1), nil, nil},
	} {
		got := gapToTarget(c.direction, c.best, c.target)
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("%s best %v target %v: gap %v, want %v", c.direction, c.best, c.target, got, c.want)
		}
	}
	cases := []struct {
		metric   domain.MetricDefinition
		baseline float64
		want     *float64
	}{
		{domain.MetricDefinition{Direction: "maximize", TargetVsBaseline: f(0.5)}, 2, f(3)},
		{domain.MetricDefinition{Direction: "maximize", TargetVsBaseline: f(0.5)}, -2, f(-1)},
		{domain.MetricDefinition{Direction: "minimize", TargetVsBaseline: f(-0.25)}, 4, f(3)},
		{domain.MetricDefinition{Direction: "minimize", TargetVsBaseline: f(-0.25)}, -4, f(-5)},
		{domain.MetricDefinition{Direction: "maximize", TargetVsBaseline: f(0.5)}, 0, nil},
		{domain.MetricDefinition{Direction: "maximize"}, 2, nil},
	}
	for _, c := range cases {
		got := c.metric.Target(c.baseline)
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("%s by %v against %g: target %v, want %v", c.metric.Direction, c.metric.TargetVsBaseline, c.baseline, got, c.want)
		}
	}
}

// A closed run has no minutes left whatever ends_at it kept, and says when and why it closed. A
// run not started counts no minutes and says it has not started.
func TestTheClockOfAClosedOrNotStartedRunCountsNoTimeLeft(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now.Add(-time.Hour)
	closed := clockOf(&domain.PlatformExperiment{Status: domain.PlatformExpClosed, EndsAt: now.Add(3 * time.Hour), ClosedAt: &closedAt, CloseReason: domain.CloseByRequest}, now)
	if closed.MinutesLeft == nil || *closed.MinutesLeft != 0 || closed.EndsAt != nil || closed.ClosedAt == nil || closed.CloseReason != "closed_by_request" || closed.NotStarted {
		t.Fatalf("closed: %+v, want minutes_left 0, closed_at and close_reason, no ends_at", closed)
	}
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpDraft} {
		open := clockOf(&domain.PlatformExperiment{Status: status, DurationHours: 4}, now)
		if !open.NotStarted || open.MinutesLeft != nil || open.EndsAt != nil || open.DurationHours == nil || *open.DurationHours != 4 {
			t.Fatalf("%s: %+v, want not_started with its duration and no minutes counted", status, open)
		}
	}
	running := clockOf(&domain.PlatformExperiment{Status: domain.PlatformExpRunning, EndsAt: now.Add(30 * time.Minute)}, now)
	if running.MinutesLeft == nil || *running.MinutesLeft < 29 || running.EndsAt == nil || running.NotStarted || running.ClosedAt != nil {
		t.Fatalf("running: %+v", running)
	}
}

// A run's record said its lead "did not finish: its machine ended (gone)" though a
// replacement worked to the close, and "released" with "no activity reported" for machines the
// close itself stopped. An end is reported only of a machine that ended before the close and
// was not replaced.
func TestRunEndedReportsOnlyAnEndBeforeTheCloseThatNothingReplaced(t *testing.T) {
	closedAt := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	asked, before, after := closedAt.Add(-5*time.Hour), closedAt.Add(-2*time.Hour), closedAt.Add(time.Minute)
	for _, c := range []struct {
		name     string
		instance *domain.AgentInstance
		want     string
	}{
		{"replaced and running", &domain.AgentInstance{AskedAt: &asked, Name: "m-2", Reported: domain.InstanceRunning, EndReason: "gone", ExitedAt: &before}, "did not finish: at work at the close"},
		{"replaced and starting", &domain.AgentInstance{AskedAt: &asked, Name: "m-2", Reported: domain.InstanceStarting, EndReason: "exited", ExitedAt: &before}, "did not finish: its machine was starting at the close"},
		{"released by the close", &domain.AgentInstance{AskedAt: &asked, EndReason: "released", ExitedAt: &after, Released: true}, "did not finish: at work at the close"},
		{"ended at the close", &domain.AgentInstance{AskedAt: &asked, EndReason: "exited", ExitedAt: &closedAt}, "did not finish: at work at the close"},
		{"ended before, not replaced", &domain.AgentInstance{AskedAt: &asked, EndReason: "silent", ExitedAt: &before}, "did not finish: its machine ended (silent) at 2026-10-09T16:00:00Z, and none ran after it"},
		{"stopped", &domain.AgentInstance{AskedAt: &asked, EndReason: "exited", ExitedAt: &before, Stopped: true}, "did not finish: its machine ended (exited) at 2026-10-09T16:00:00Z with no restart left, and none ran after it"},
		{"never asked", &domain.AgentInstance{}, "never launched"},
	} {
		if got := runEnded(c.instance, nil, "", closedAt); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := runEnded(nil, &before, "quota spent", closedAt); got != `finished at 2026-10-09T16:00:00Z: "quota spent"` {
		t.Errorf("finished: %q", got)
	}
}

// The record of a close waits for every job to settle, and by then the platform released the
// machines and cleared what their agents last said. It states the agents as they stood at the
// close.
func TestTheRecordOfACloseStatesTheAgentsAsTheyStoodAtTheClose(t *testing.T) {
	now := time.Now().UTC()
	asked, exited := now.Add(-3*time.Hour), now.Add(-2*time.Hour)
	pe := &domain.PlatformExperiment{ID: "pe-ended", ProjectID: "prj-a", Status: domain.PlatformExpRunning, CreatedAt: now.Add(-4 * time.Hour),
		StartedAt: now.Add(-3 * time.Hour), EndsAt: now.Add(-time.Second), DurationHours: 3, BudgetAcceleratorHours: 6, Stages: []domain.Stage{{LengthPct: 100}}}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{pe}, baselines: []domain.AgentSignup{{AgentID: "ag-1", Name: "Researcher 1", Role: domain.SignupRoleCompetitor, Hosted: true,
		Instance: &domain.AgentInstance{AskedAt: &asked, Name: "m-2", Reported: domain.InstanceRunning, EndReason: "gone", ExitedAt: &exited},
		Activity: &domain.AgentActivity{Text: "running hl job submit", At: now.Add(-time.Minute)}}}},
		unsettled: map[string]bool{"pe-ended": true}}
	svc := NewPlatformExperimentsService(store, platformexperimentstest.Settings, zap.NewNop(), platformexperimentstest.NoUsage(t), time.Minute)
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pe.Status != domain.PlatformExpClosed || store.records[pe.ID] != "" {
		t.Fatalf("status %s, record %q, want closed and the record waiting for its jobs", pe.Status, store.records[pe.ID])
	}
	released := now.Add(time.Minute)
	store.baselines[0].Instance = &domain.AgentInstance{AskedAt: &asked, EndReason: "released", ExitedAt: &released, Released: true}
	store.baselines[0].Activity = nil
	store.unsettled[pe.ID] = false
	if err := svc.RecordCloses(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := store.records[pe.ID]
	if want := `- Researcher 1 (competitor): 0 jobs submitted; last activity "running hl job submit" since`; !strings.Contains(record, want) || !strings.Contains(record, "did not finish: at work at the close.") {
		t.Fatalf("record %q, want the activity and the run as they stood at the close", record)
	}
	if strings.Contains(record, "released") || strings.Contains(record, "gone") || strings.Contains(record, "no activity reported") {
		t.Fatalf("record %q states an end that is not how the run stood at the close", record)
	}
}

// situationStore is statusStore with the reads the situation and the idle notice make.
type situationStore struct {
	statusStore
	jobs      map[string]*domain.Experiment
	quotas    []*domain.AgentQuota
	summaryAt *time.Time
	allowance map[string]domain.MessageAllowance
	readers   []domain.Actor
	inFlight  []domain.HypothesisInFlight
}

func (s *situationStore) GetExperiment(_ context.Context, id string) (*domain.Experiment, error) {
	return s.jobs[id], nil
}

func (s *situationStore) ListAgentQuotas(context.Context, string) ([]*domain.AgentQuota, error) {
	return s.quotas, nil
}

func (s *situationStore) AddDesiredQuotaUsage(context.Context, string, []*domain.AgentQuota) error {
	return nil
}

func (s *situationStore) GetAgentRunningExperiments(context.Context, string, string) ([]*domain.Experiment, error) {
	return nil, nil
}

func (s *situationStore) ListCutAgents(context.Context, string) ([]domain.AgentCut, error) {
	return nil, nil
}

func (s *situationStore) MessageAllowance(_ context.Context, _ string, reader domain.Actor) (map[string]domain.MessageAllowance, error) {
	s.readers = append(s.readers, reader)
	return s.allowance, nil
}

func (s *situationStore) SummaryWrittenAt(context.Context, string) (*time.Time, error) {
	return s.summaryAt, nil
}

func (s *situationStore) HypothesesInFlight(context.Context, string) ([]domain.HypothesisInFlight, error) {
	return s.inFlight, nil
}

func (s *situationStore) CompletedJobIDs(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// personIdeas holds how many open ideas of people each platform experiment has.
type personIdeas map[string]int

func (p personIdeas) OpenPersonIdeas(_ context.Context, pe string) (int, error) { return p[pe], nil }

// situationRun is a running platform experiment of two researchers, one with a job running and
// one with none for 25 minutes, whose baseline job takes two devices of typeA on one node.
func situationRun(t *testing.T) (*PlatformExperimentsService, *situationStore) {
	t.Helper()
	now := time.Now().UTC()
	idleSince, summaryAt := now.Add(-25*time.Minute), now.Add(-time.Hour)
	pe := &domain.PlatformExperiment{ID: "pe-1", ProjectID: "prj-a", Status: domain.PlatformExpRunning, CreatedAt: now.Add(-5 * time.Hour),
		StartedAt: now.Add(-3 * time.Hour), EndsAt: now.Add(90 * time.Minute), DurationHours: 4.5, BudgetAcceleratorHours: 100,
		Stages: []domain.Stage{{LengthPct: 100}}, CurrentStage: 1, BaselineJobID: "job-base", CoordinatorHosted: true,
		Coordinator: &domain.HostedCoordinator{}, Metrics: []domain.MetricDefinition{{Key: "acc", Direction: "maximize"}}}
	store := &situationStore{
		statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{pe}, researchers: []string{}, baselines: []domain.AgentSignup{
			{AgentID: "ag-1", Name: "Researcher 1", Role: domain.SignupRoleCompetitor, InFlight: &domain.InFlight{Jobs: 2, Queued: 1, Running: 1}},
			{AgentID: "ag-2", Name: "Researcher 2", Role: domain.SignupRoleCompetitor, InFlight: &domain.InFlight{IdleSince: &idleSince}},
		}}},
		jobs: map[string]*domain.Experiment{"job-base": {ID: "job-base", AcceleratorType: typeA, AcceleratorCount: 2,
			Job: domain.JobSpec{AcceleratorType: typeA, AcceleratorCount: 2, CPU: "4"}}},
		quotas: []*domain.AgentQuota{
			{AgentID: "ag-1", PlatformExperimentID: "pe-1", GuaranteedAcceleratorHours: 50},
			{AgentID: "ag-2", PlatformExperimentID: "pe-1", GuaranteedAcceleratorHours: 50},
		},
		summaryAt: &summaryAt,
		allowance: map[string]domain.MessageAllowance{"ag-1": {LeftThisHour: 6}, "ag-2": {LeftThisHour: 4}},
		inFlight:  []domain.HypothesisInFlight{{HypothesisID: "hyp-1", Queued: 1, Running: 1, AgentIDs: []string{"ag-1", "ag-gone"}}},
	}
	svc := NewPlatformExperimentsService(store, platformexperimentstest.Settings, zap.NewNop(), platformexperimentstest.NoUsage(t), time.Minute).WithPersonIdeas(personIdeas{"pe-1": 2})
	svc.placement = func(context.Context, string) (*Placement, error) {
		return &Placement{
			Clusters: []ClusterCapacity{{ClusterName: "c1", Accelerators: []AcceleratorCapacity{
				{AcceleratorType: string(typeA), Kind: domain.TypeKind("device"), Available: 6, Total: 8},
				{AcceleratorType: "example.com/gpu=b", Available: 8, Total: 8},
			}}},
			NodeAccelerators: map[string]map[string]map[string]int64{"c1": {"n1": {string(typeA): 4}, "n2": {string(typeA): 2}}},
			NodeResources:    map[string]map[string]map[string]int64{"c1": {"n1": {domain.NodeResourceCPUMillicores: 64000}, "n2": {domain.NodeResourceCPUMillicores: 64000}}},
		}, nil
	}
	return svc, store
}

// An agent's token reads the run and its own row; a coordinator's reads every row and the hosted
// coordinator. Capacity counts the run's jobs of every agent either way.
func TestTheSituationAnswersTheRunAndTheRowsTheCallerMaySee(t *testing.T) {
	svc, _ := situationRun(t)
	ctx := context.Background()
	all, err := svc.Situation(ctx, "pe-1", "", domain.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Run.MinutesLeft == nil || *all.Run.MinutesLeft < 88 || *all.Run.MinutesLeft > 90 || all.Run.Stage == nil || all.Run.Stage.Index != 1 || all.Run.Stage.Of != 1 {
		t.Fatalf("run: %+v", all.Run)
	}
	if len(all.Capacity.Clusters) != 1 || all.Capacity.Clusters[0].AcceleratorType != string(typeA) || all.Capacity.Clusters[0].Free != 6 ||
		all.Capacity.FitsNow == nil || *all.Capacity.FitsNow != 3 || all.Capacity.JobsInFlightRun != (JobCounts{Queued: 1, Running: 1}) {
		t.Fatalf("capacity: %+v, want the baseline type's line, 3 two-device jobs and the run's two jobs", all.Capacity)
	}
	if all.Budget.RunBudgetAccH != 100 || len(all.Objective) != 1 || all.Objective[0].Metric != "acc" {
		t.Fatalf("budget %+v, objective %+v", all.Budget, all.Objective)
	}
	if len(all.Agents) != 2 || all.Coordinator == nil || all.Coordinator.SummaryWrittenAt == nil {
		t.Fatalf("coordinator's read: agents %+v, coordinator %+v, want both rows and the coordinator", all.Agents, all.Coordinator)
	}
	idle := all.Agents[1]
	if idle.AgentID != "ag-2" || idle.MinutesWithoutJobInFlight == nil || *idle.MinutesWithoutJobInFlight != 25 || idle.Messages == nil || idle.Messages.LeftThisHour != 4 ||
		idle.Budget == nil || idle.Budget.Share != 50 || idle.Budget.RoomGuaranteed != 50 {
		t.Fatalf("idle row: %+v", idle)
	}
	if busy := all.Agents[0]; busy.Jobs != (JobCounts{Queued: 1, Running: 1}) || *busy.MinutesWithoutJobInFlight != 0 {
		t.Fatalf("busy row: %+v", busy)
	}
	if len(all.InFlight) != 1 || all.InFlight[0].HypothesisID != "hyp-1" || all.InFlight[0].Queued != 1 || all.InFlight[0].Running != 1 ||
		!slices.Equal(all.InFlight[0].Agents, []string{"Researcher 1", "ag-gone"}) {
		t.Fatalf("hypotheses in flight: %+v, want hyp-1's two jobs by name, an agent without a row by id", all.InFlight)
	}
	if want := (SituationIdeas{Open: 2, List: "hl hypothesis list --by people"}); all.Ideas != want {
		t.Fatalf("ideas from people: %+v, want %+v", all.Ideas, want)
	}

	own, err := svc.Situation(ctx, "pe-1", "ag-2", domain.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(own.Agents) != 1 || own.Agents[0].AgentID != "ag-2" || own.Coordinator != nil || own.InFlight != nil || own.Capacity.JobsInFlightRun != (JobCounts{Queued: 1, Running: 1}) {
		t.Fatalf("agent's read: agents %+v, coordinator %+v, capacity %+v", own.Agents, own.Coordinator, own.Capacity)
	}
	if own.Ideas.Open != 2 {
		t.Fatalf("agent's read: ideas from people %+v, want the run's 2", own.Ideas)
	}
	if _, err := svc.Situation(ctx, "pe-none", "", domain.Actor{}); !errors.Is(err, ErrPlatformExperimentNotFound) {
		t.Fatalf("no such run: %v", err)
	}
}

// The messages left in the situation are counted for the reader: the sender of the actor that reads it.
func TestTheSituationCountsTheMessagesItsReaderMaySend(t *testing.T) {
	svc, store := situationRun(t)
	owner := domain.Actor{Kind: domain.ActorUser, ID: "user-1"}
	if _, err := svc.Situation(context.Background(), "pe-1", "", owner); err != nil {
		t.Fatal(err)
	}
	if len(store.readers) == 0 || store.readers[len(store.readers)-1] != owner {
		t.Fatalf("allowance read for %+v, want the owner who reads the situation", store.readers)
	}
}

// idleNotices hands out the researchers due a notice and keeps what each was told.
type idleNotices struct {
	due  []db.IdleResearcher
	told map[string]string
	free []string
}

func (n *idleNotices) IdleResearchers(context.Context, time.Duration) ([]db.IdleResearcher, error) {
	return n.due, nil
}

func (n *idleNotices) NoticeIdle(_ context.Context, r db.IdleResearcher, detail string) error {
	n.told[r.PlatformExperimentID+"/"+r.AgentID] = detail
	return nil
}

func (n *idleNotices) MarkCapacityFree(_ context.Context, id string, free bool, _ time.Duration) (*db.IdleCapacity, error) {
	n.free = append(n.free, fmt.Sprintf("%s=%t", id, free))
	if !free {
		return nil, nil
	}
	return &db.IdleCapacity{PlatformExperimentID: id, Since: time.Now().Add(-12*time.Minute - 30*time.Second)}, nil
}

func (n *idleNotices) NoticeCapacityIdle(_ context.Context, r db.IdleCapacity, detail string) error {
	n.told[r.PlatformExperimentID] = detail
	return nil
}

// A run's places stood free while each researcher had a job in flight, and the coordinator was
// told nothing: the harness's own check went silent once the target was passed. The sweep tells
// whoever steers it of the places free beyond the queued jobs, and a run with none free or no
// time left ends its stretch.
func TestACapacityIdleNoticeSaysThePlacesFreeBeyondTheQueuedJobs(t *testing.T) {
	svc, store := situationRun(t)
	notices := &idleNotices{told: map[string]string{}}
	svc.WithIdleNotice(notices, 10*time.Minute)
	if err := svc.NoticeIdleCapacity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(notices.free, []string{"pe-1=true"}) {
		t.Fatalf("marked %v, want pe-1 free: 3 places beside 1 queued", notices.free)
	}
	detail := notices.told["pe-1"]
	for _, want := range []string{"the free capacity places 2 more jobs shaped like the baseline job beyond the 1 queued, and has for 12 minutes",
		"1 jobs of the run are queued and 1 running", "minutes of the run are left", "no best is measured yet"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q, want it to say %q", detail, want)
		}
	}

	store.created[0].EndsAt = time.Now().Add(-time.Minute)
	notices.free, notices.told = nil, map[string]string{}
	if err := svc.NoticeIdleCapacity(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(notices.free, []string{"pe-1=false"}) || len(notices.told) != 0 {
		t.Fatalf("no time left: marked %v, told %v, want the stretch ended and nothing told", notices.free, notices.told)
	}
}

// A coordinator was told of free places only while the best was short of the target, and the
// places stood free for 40 minutes past it. The notice says where the best stands either way.
func TestACapacityIdleNoticeSaysTheBestShortOfOrPastTheTarget(t *testing.T) {
	best, target := 0.62, 0.8
	short, past := 22.5, -12.5
	left := 200
	notice := capacityNotice{free: 7, minutes: 15, jobs: JobCounts{Queued: 1, Running: 2}, minutesLeft: &left,
		primary: &SituationMetric{Metric: "ratio", BestValue: &best, Target: &target, GapToTargetPct: &short}}
	if got, want := notice.detail(), "the free capacity places 7 more jobs shaped like the baseline job beyond the 1 queued, and has for 15 minutes; "+
		"1 jobs of the run are queued and 2 running; 200 minutes of the run are left; the best ratio is 0.62, 22.5% short of the target 0.8"; got != want {
		t.Errorf("short of the target:\n got %q\nwant %q", got, want)
	}
	best = 0.9
	notice.primary.GapToTargetPct = &past
	if got := notice.detail(); !strings.HasSuffix(got, "the best ratio is 0.9, 12.5% past the target 0.8") {
		t.Errorf("past the target: %q", got)
	}
	notice.primary.GapToTargetPct, notice.primary.Target = nil, nil
	if got := notice.detail(); !strings.HasSuffix(got, "the best ratio is 0.9, against no target") {
		t.Errorf("no target: %q", got)
	}
}

// A lead told once that it was idle rested from mid-run to the close, and the notice
// said nothing of what was free. Each notice carries what the platform knows then, and a run
// whose numbers cannot be read is skipped without keeping the others from being told.
func TestAnIdleNoticeSaysWhatThePlatformKnowsAtTheNotice(t *testing.T) {
	svc, _ := situationRun(t)
	notices := &idleNotices{told: map[string]string{}, due: []db.IdleResearcher{
		{PlatformExperimentID: "pe-gone", AgentID: "ag-9", Since: time.Now().Add(-time.Hour)},
		{PlatformExperimentID: "pe-1", AgentID: "ag-2", Since: time.Now().Add(-25 * time.Minute), Repeat: 2},
	}}
	svc.WithIdleNotice(notices, 10*time.Minute)
	if err := svc.NoticeIdleResearchers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(notices.told) != 1 {
		t.Fatalf("told %v, want the researcher of the run that reads", notices.told)
	}
	detail := notices.told["pe-1/ag-2"]
	for _, want := range []string{"no job of its own ran on the platform for 25 minutes, and it has none queued or running", "its harness reports no state",
		"the free capacity places 3 jobs shaped like the baseline job now", "minutes of the run are left", "50 of its share of 50 accelerator hours are left"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q, want it to say %q", detail, want)
		}
	}
	turn := idleNotice{minutes: 12, subagents: new(int), turn: &domain.AgentTurn{State: domain.TurnWaiting, Since: time.Date(2026, 10, 9, 14, 5, 0, 0, time.UTC)}}
	if got := turn.detail(); got != "no job of its own ran on the platform for 12 minutes, and it has none queued or running; its harness reports 0 subagents running and its turn waiting since 2026-10-09T14:05:00Z" {
		t.Errorf("detail with the harness's state: %q", got)
	}
}

// A harness reading speaks for the machine the agent holds now only: one read before that
// machine was asked for, or of a machine that ended, says nothing of the agent.
func TestAHarnessReadingCountsOnlyForTheMachineHeldNow(t *testing.T) {
	asked := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	state := domain.HarnessState{At: asked.Add(time.Minute)}
	if !liveReading(&domain.AgentInstance{Name: "m-2", AskedAt: &asked}, state) {
		t.Error("a reading of the machine held now is not taken")
	}
	if liveReading(&domain.AgentInstance{Name: "m-2", AskedAt: &asked}, domain.HarnessState{At: asked.Add(-time.Minute)}) {
		t.Error("a reading of the machine before is taken")
	}
	if liveReading(&domain.AgentInstance{AskedAt: &asked, EndReason: "exited"}, state) || liveReading(&domain.AgentInstance{Name: "m-2", AskedAt: &asked, Released: true}, state) || liveReading(nil, state) {
		t.Error("a reading of a machine no longer held is taken")
	}
	resets := asked.Add(time.Hour)
	allowance := domain.ModelAllowanceOf(domain.RateLimit{Rejected: true, ResetsAt: &resets}, asked)
	if allowance.RefusedUntil == nil || !allowance.RefusedUntil.Equal(resets) {
		t.Errorf("refused: %+v", allowance)
	}
	if allowance := domain.ModelAllowanceOf(domain.RateLimit{Rejected: true, ResetsAt: &resets}, resets.Add(time.Second)); allowance.RefusedUntil != nil {
		t.Errorf("a refusal past its reset: %+v", allowance)
	}
	// A coordinator pacing the run reads how far each account is into its window and when it resets.
	used := 0.64
	allowance = domain.ModelAllowanceOf(domain.RateLimit{Utilization: &used, ResetsAt: &resets}, asked)
	if allowance.RefusedUntil != nil || allowance.Utilization == nil || *allowance.Utilization != used || allowance.ResetsAt == nil || !allowance.ResetsAt.Equal(resets) {
		t.Errorf("allowed: %+v", allowance)
	}
	if allowance := domain.ModelAllowanceOf(domain.RateLimit{Utilization: &used, ResetsAt: &resets}, resets.Add(time.Second)); allowance.Utilization != nil || allowance.ResetsAt != nil {
		t.Errorf("a reading of a window that reset: %+v", allowance)
	}
}

// The situation says which metric is declared deterministic, the one place every wake reads.
func TestTheSituationSaysAMetricIsDeterministic(t *testing.T) {
	now := time.Now()
	if !metricStanding(domain.MetricDefinition{Key: "ratio", Direction: "maximize", Deterministic: true}, nil, now).Deterministic {
		t.Fatal("a metric declared deterministic reads as not")
	}
	if metricStanding(domain.MetricDefinition{Key: "acc", Direction: "maximize"}, nil, now).Deterministic {
		t.Fatal("a metric not declared deterministic reads as one")
	}
}

// A coordinator ended a review with "nothing to act on" while results had come in that its summary
// did not hold. The situation counts the jobs completed since the summary was last written.
func TestTheSituationCountsTheJobsCompletedSinceTheSummary(t *testing.T) {
	svc, store := situationRun(t)
	before, after := store.summaryAt.Add(-time.Minute), store.summaryAt.Add(time.Minute)
	store.completed = []*domain.Experiment{{ID: "job-1", AgentID: "ag-1", FinishedAt: &before}, {ID: "job-2", AgentID: "ag-2", FinishedAt: &after}}
	got, err := svc.Situation(context.Background(), "pe-1", "", domain.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Coordinator == nil || got.Coordinator.JobsCompletedSinceSummary != 1 {
		t.Fatalf("coordinator %+v, want the one job completed after the summary", got.Coordinator)
	}
	store.summaryAt = nil
	if got, err = svc.Situation(context.Background(), "pe-1", "", domain.Actor{}); err != nil || got.Coordinator.JobsCompletedSinceSummary != 2 {
		t.Fatalf("coordinator %+v (%v), want every job while no summary was written", got.Coordinator, err)
	}
}

// A researcher's subagents audited and submitted on an obsolete stack, and nothing they read named
// the commit the best ran. The situation, which every wake carries, names it and its branch.
func TestTheSituationNamesTheCommitOfTheBest(t *testing.T) {
	svc, store := situationRun(t)
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.FormValue("query"), `metric_name="acc"`) {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
				`{"metric":{"agent_id":"ag-1","job_id":"job-7","metric_basis":"raw"},"value":[0,"0.9"]},` +
				`{"metric":{"agent_id":"ag-2","job_id":"job-8","metric_basis":"raw"},"value":[0,"0.8"]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(metrics.Close)
	svc.metricsDBURL, store.researchers = metrics.URL, []string{"ag-1", "ag-2"}
	done := time.Now().Add(-time.Minute)
	store.completed = []*domain.Experiment{
		{ID: "job-7", AgentID: "ag-1", FinishedAt: &done, CodeRef: "https://example.com/repo.git@f9a3", Branch: "hl/pe-1/r1-stack"},
		{ID: "job-8", AgentID: "ag-2", FinishedAt: &done, CodeRef: "https://example.com/repo.git@0ld0", Branch: "hl/pe-1/r2"},
	}
	got, err := svc.Situation(context.Background(), "pe-1", "ag-2", domain.Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if m := got.Objective[0]; m.BestJob != "job-7" || m.BestCodeRef != "https://example.com/repo.git@f9a3" || m.BestBranch != "hl/pe-1/r1-stack" {
		t.Fatalf("objective %+v, want job-7 with its commit and branch", m)
	}
}
