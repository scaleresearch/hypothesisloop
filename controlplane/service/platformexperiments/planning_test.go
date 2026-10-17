package platformexperiments

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/gitrefs"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// An accepted draft waited for a baseline nobody could measure, with its coordinator's machine
// rented until the run's end time, and a run of one hour closed end_time_reached while its
// baseline was still being prepared. An open experiment has until its baseline_due_at to be
// running and is closed then, with a baseline or without. The run's length is no part of it.
func TestAnOpenExperimentNotRunningByItsBaselineDeadlineIsClosed(t *testing.T) {
	now := time.Now().UTC()
	open := func(id string, due time.Time, baseline string) *domain.PlatformExperiment {
		return &domain.PlatformExperiment{ID: id, ProjectID: "prj-a", Status: domain.PlatformExpOpen, CreatedAt: now.Add(-48 * time.Hour),
			DurationHours: 1, BaselineDueAt: due, BaselineJobID: baseline, Stages: []domain.Stage{{LengthPct: 100}}}
	}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{
		open("pe-late", now.Add(-time.Minute), ""),
		open("pe-declared-late", now.Add(-time.Minute), "job-1"),
		open("pe-in-time", now.Add(time.Minute), ""),
		open("pe-declared-in-time", now.Add(time.Minute), "job-1"),
	}}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, pe := range store.created {
		want := domain.PlatformExpOpen
		if strings.HasSuffix(pe.ID, "-late") {
			want = domain.PlatformExpClosed
		}
		if pe.Status != want || (want == domain.PlatformExpClosed && pe.ClosedBy != nil) {
			t.Errorf("%s: status %s closed by %v, want %s by the platform", pe.ID, pe.Status, pe.ClosedBy, want)
		}
	}
}

// A run closed 59 s after its ends_at, on the minute tick of the expiry sweep. SweepEnded, on
// its own short tick, closes a running one past its end and leaves what the minute tick tends.
func TestSweepEndedClosesARunPastItsEndAndNothingElse(t *testing.T) {
	now := time.Now().UTC()
	ended := &domain.PlatformExperiment{ID: "pe-ended", ProjectID: "prj-a", Status: domain.PlatformExpRunning, CreatedAt: now.Add(-2 * time.Hour),
		StartedAt: now.Add(-time.Hour), EndsAt: now.Add(-time.Second), DurationHours: 1, BudgetAcceleratorHours: 6, Stages: []domain.Stage{{LengthPct: 100}}}
	going := &domain.PlatformExperiment{ID: "pe-going", ProjectID: "prj-a", Status: domain.PlatformExpRunning, CreatedAt: now.Add(-2 * time.Hour),
		StartedAt: now.Add(-time.Hour), EndsAt: now.Add(time.Minute), DurationHours: 1, BudgetAcceleratorHours: 6, Stages: []domain.Stage{{LengthPct: 100}}}
	late := &domain.PlatformExperiment{ID: "pe-late", ProjectID: "prj-a", Status: domain.PlatformExpOpen, CreatedAt: now.Add(-time.Hour),
		DurationHours: 1, BudgetAcceleratorHours: 6, BaselineDueAt: now.Add(-time.Minute), Stages: []domain.Stage{{LengthPct: 100}}}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{ended, going, late}}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
	if err := svc.SweepEnded(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ended.Status != domain.PlatformExpClosed || going.Status != domain.PlatformExpRunning || late.Status != domain.PlatformExpOpen {
		t.Fatalf("got ended %s, going %s, late %s, want closed, running and open", ended.Status, going.Status, late.Status)
	}
}

// A coordinator that rewrites its summary after the close from the one it read back, the record
// included, leaves the record once, after its words.
func TestASummaryWrittenWithTheRecordInItKeepsTheRecordOnce(t *testing.T) {
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-ended", ProjectID: "prj-a", Status: domain.PlatformExpRunning, CreatedAt: now.Add(-2 * time.Hour),
		StartedAt: now.Add(-time.Hour), EndsAt: now.Add(-time.Second), DurationHours: 1, BudgetAcceleratorHours: 6, Stages: []domain.Stage{{LengthPct: 100}}}
	store := &summarizedStore{statusStore: statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{pe}}}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := pe.Summary
	if _, err := svc.SetSummary(context.Background(), pe.ID, "The run found it.\n\n"+record); err != nil {
		t.Fatal(err)
	}
	if pe.Summary != "The run found it.\n\n"+record {
		t.Fatalf("summary = %q, want the coordinator's words and the record once", pe.Summary)
	}
}

// A run closed at its baseline deadline with no coordinator ever started: its summary stayed
// empty and the close message did not say that no job was ever submitted.
func TestAnExperimentThatClosesWithNoSummaryIsGivenTheRecordOfItsClose(t *testing.T) {
	now := time.Now().UTC()
	asked := now.Add(-10 * time.Minute)
	pe := &domain.PlatformExperiment{ID: "pe-late", ProjectID: "prj-a", Status: domain.PlatformExpOpen, CreatedAt: now.Add(-time.Hour),
		DurationHours: 1, BudgetAcceleratorHours: 18, BaselineDueAt: now.Add(-time.Minute), Stages: []domain.Stage{{LengthPct: 100}},
		CoordinatorInAgentSandbox: true, Coordinator: &domain.AgentSandboxCoordinator{Instance: domain.AgentInstance{State: domain.InstanceWaiting}}}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{pe}, researchers: []string{"ag-researcher"},
		baselines: []domain.AgentSignup{{AgentID: "ag-baseline", Name: "baseline", Role: domain.SignupRoleBaseline,
			Instance: &domain.AgentInstance{AskedAt: &asked, Name: "m-1", Reported: domain.InstanceRunning}, Activity: &domain.AgentActivity{Text: "writing harness/run_trial.py", At: asked}}}}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"baseline_limit_reached", "no job was ever submitted", "Jobs submitted: 0.", "Spent: 0 of the budget of 18",
		"- coordinator: no activity reported; never launched: the platform launches a coordinator once the run starts or the experiment waits for it, and this one closed before either, so this record is its summary.", `- baseline (baseline): 0 jobs submitted; last activity "writing harness/run_trial.py"`, "did not finish: at work at the close", "(competitor): 0 jobs submitted; no activity reported; never launched.", "No job."} {
		if !strings.Contains(pe.Summary, want) {
			t.Errorf("summary = %q, want it to say %q", pe.Summary, want)
		}
	}
}

// A coordinator wrote a summary mid-run ("Status: running") and its rewrite at the close
// was cut off: the close record was skipped because a summary existed, so the stale one stood
// alone. The platform writes its record of every close, after whatever summary there is.
func TestTheRecordOfACloseFollowsASummaryWrittenBefore(t *testing.T) {
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-ended", ProjectID: "prj-a", Status: domain.PlatformExpRunning, CreatedAt: now.Add(-2 * time.Hour),
		StartedAt: now.Add(-time.Hour), EndsAt: now.Add(-time.Second), DurationHours: 1, BudgetAcceleratorHours: 6,
		Stages: []domain.Stage{{LengthPct: 100}}, Summary: "Status: running. Researchers are at work.",
		BaselineJobID: "job-2", BaselineRaises: []domain.BaselineRaise{{From: "job-1", To: "job-2", Reason: "job-2 measured the control's other coder",
			Actor: domain.Actor{Kind: domain.ActorCoordinator, ID: "pe-ended"}, At: time.Date(2026, 10, 8, 19, 0, 0, 0, time.UTC)}}}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{pe}, messages: []domain.AgentMessage{{AgentID: "ag-1", From: "coordinator",
		Text: "leave the lr line,\ntry warmup", At: time.Date(2026, 10, 8, 19, 30, 0, 0, time.UTC)}}}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pe.Status != domain.PlatformExpClosed || !strings.HasPrefix(pe.Summary, "Status: running. Researchers are at work.\n\nRecorded by the platform: it closed at ") {
		t.Fatalf("status %s, summary %q, want closed with the record after the summary written before", pe.Status, pe.Summary)
	}
	// The record says who raised the baseline while it ran, and why.
	if want := "Baseline raised while it ran:\n- at 2026-10-08T19:00:00Z, from job job-1 to job job-2, by coordinator pe-ended: job-2 measured the control's other coder.\n"; !strings.Contains(pe.Summary, want) {
		t.Errorf("summary = %q, want it to say %q", pe.Summary, want)
	}
	// And every message an agent was sent.
	if want := "Messages to agents:\n- at 2026-10-08T19:30:00Z, from the coordinator to ag-1: leave the lr line, try warmup\n"; !strings.Contains(pe.Summary, want) {
		t.Errorf("summary = %q, want it to say %q", pe.Summary, want)
	}
}

// An owner who closes a run early says why, and the record of the close and its status_reason,
// which a coordinator on its last turn reads, say it too.
func TestAnOwnersReasonToCloseIsRecorded(t *testing.T) {
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-early", ProjectID: "prj-a", Status: domain.PlatformExpRunning, CreatedAt: now.Add(-2 * time.Hour),
		StartedAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), DurationHours: 2, BudgetAcceleratorHours: 6, Stages: []domain.Stage{{LengthPct: 100}}}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{pe}}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute)
	owner := domain.Actor{Kind: domain.ActorUser, ID: "user-1"}
	closed, err := svc.Close(context.Background(), pe.ID, &owner, "the baseline is wrong:\nit measured the old split", nil)
	if err != nil {
		t.Fatal(err)
	}
	said := `named user-1 asked for it to be closed, saying "the baseline is wrong: it measured the old split"`
	if want := "Close reason: closed_by_request: " + said + ".\n"; !strings.Contains(closed.Summary, want) {
		t.Errorf("summary = %q, want it to say %q", closed.Summary, want)
	}
	why, err := svc.closedReason(context.Background(), closed)
	if err != nil || why.Message != said {
		t.Errorf("status_reason = %+v, %v, want the message %q", why, err, said)
	}
	closed.ClosedBy, closed.CloseMessage = nil, ""
	why, err = svc.closedReason(context.Background(), closed)
	if want := "the owner asked for it to be closed"; err != nil || why.Message != want {
		t.Errorf("status_reason without a recorded closer = %+v, %v, want %q", why, err, want)
	}
}

// A close record said "Spent: 0 of the budget" while consumed_accelerator_hours read 0.0645: the
// record was written at the close, before the job its cluster was still starting ended and was
// billed. The record of a close waits for every job to be settled, and states the spend whole.
func TestTheRecordOfACloseWaitsForItsJobsToBeBilled(t *testing.T) {
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-late", ProjectID: "prj-a", Status: domain.PlatformExpOpen, CreatedAt: now.Add(-time.Hour),
		DurationHours: 1, BudgetAcceleratorHours: 6, BaselineDueAt: now.Add(-time.Minute), Stages: []domain.Stage{{LengthPct: 100}}}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{pe}}, unsettled: map[string]bool{pe.ID: true}}
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"0.0645"]}]}}`))
	}))
	t.Cleanup(metrics.Close)
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(metrics.URL), time.Minute)
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordCloses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pe.Status != domain.PlatformExpClosed || pe.Summary != "" {
		t.Fatalf("closed with a job still live: status %s, summary %q, want closed with no record yet", pe.Status, pe.Summary)
	}
	store.unsettled[pe.ID] = false
	if err := svc.RecordCloses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pe.Summary, "Spent: 0.0645 of the budget of 6 accelerator hours.") {
		t.Errorf("summary = %q, want the spend once the job was billed", pe.Summary)
	}
}

// A run closed at its baseline deadline with a job its cluster could not start, an environment
// built for it and a commit on the baseline branch, and its record named none of them: the next
// run started from nothing. The record of such a close says what the baseline left.
func TestTheRecordOfABaselineDeadlineCloseSaysWhatTheBaselineLeft(t *testing.T) {
	now := time.Now().UTC()
	pe := &domain.PlatformExperiment{ID: "pe-late", ProjectID: "prj-a", Status: domain.PlatformExpOpen, CreatedAt: now.Add(-time.Hour),
		DurationHours: 1, BudgetAcceleratorHours: 9, BaselineDueAt: now.Add(-time.Minute), Stages: []domain.Stage{{LengthPct: 100}}}
	branch := gitrefs.BaselineBranch(pe.ID)
	job := &domain.Experiment{ID: "job-1", Status: domain.StatusRejected, EvictionReason: string(domain.EvictionExperimentClosed),
		InfraRequeueCount: 2, InfraRequeueReason: "stuck_pending: admitted and not running within 5m0s, its runtime reporting create_failed: secrets is forbidden",
		CodeRef: "https://github.com/acme/codec.git@4c6e96", Branch: branch, EnvironmentKey: "env-1"}
	store := &statusStore{createdStore: createdStore{created: []*domain.PlatformExperiment{pe}, objective: []*domain.Experiment{job}}}
	svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), metricsdb.New(platformexperimentstest.NoUsage(t)), time.Minute).
		WithEnvironments(builtEnvironments{"env-1": {Key: "env-1", State: domain.EnvironmentReady, Image: "registry.test/environment@sha256:ce7a"}})
	if err := svc.SweepExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"What the baseline left, on branch " + branch,
		"- job job-1: REJECTED (experiment_closed); requeued 2 times for faults of the platform's, the last stuck_pending: admitted and not running within 5m0s, its runtime reporting create_failed: secrets is forbidden; code https://github.com/acme/codec.git@4c6e96; its environment .hypothesisloop/Dockerfile built as registry.test/environment@sha256:ce7a.",
		"The last commit of " + branch + " a job was submitted from: https://github.com/acme/codec.git@4c6e96."} {
		if !strings.Contains(pe.Summary, want) {
			t.Errorf("summary = %q, want it to say %q", pe.Summary, want)
		}
	}
}

// builtEnvironments are the environments by key.
type builtEnvironments map[string]*domain.Environment

func (e builtEnvironments) Read(context.Context, string, string, string) (*string, error) {
	return nil, nil
}

func (e builtEnvironments) Get(_ context.Context, key string) (*domain.Environment, error) {
	return e[key], nil
}

// statusStore is createdStore with the reads and the write a close and a sweep make. unsettled
// are the platform experiments a job of which is live or not yet settled.
type statusStore struct {
	createdStore
	unsettled map[string]bool
	records   map[string]string
	// closeAgents are the agent lines each took before its close.
	closeAgents map[string]string
}

func (s *statusStore) NameActors(_ context.Context, actors []*domain.Actor) error {
	for _, actor := range actors {
		actor.Name = "named " + actor.ID
	}
	return nil
}

func (s *statusStore) SetCloseAgents(_ context.Context, id, lines string) error {
	for _, pe := range s.created {
		if pe.ID == id && pe.Status != domain.PlatformExpClosed {
			if s.closeAgents == nil {
				s.closeAgents = map[string]string{}
			}
			s.closeAgents[id] = lines
		}
	}
	return nil
}

func (s *statusStore) CloseAgents(_ context.Context, id string) (string, error) {
	return s.closeAgents[id], nil
}

func (s *statusStore) JobsSettled(_ context.Context, id string) (bool, error) {
	return !s.unsettled[id], nil
}

func (s *statusStore) ClosesToRecord(context.Context, time.Duration) ([]string, error) {
	var ids []string
	for _, pe := range s.created {
		if pe.Status == domain.PlatformExpClosed && s.records[pe.ID] == "" && !s.unsettled[pe.ID] {
			ids = append(ids, pe.ID)
		}
	}
	return ids, nil
}

func (s *statusStore) ListPlatformExperimentsByStatus(_ context.Context, status domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	var out []*domain.PlatformExperiment
	for _, pe := range s.created {
		if pe.Status == status {
			out = append(out, pe)
		}
	}
	return out, nil
}

func (s *statusStore) ClosePlatformExperiment(_ context.Context, id string, by *domain.Actor, message string) error {
	for _, pe := range s.created {
		if pe.ID == id {
			closedAt := time.Now().UTC()
			pe.CloseReason = domain.CloseByRequest
			if by == nil && pe.Status == domain.PlatformExpOpen {
				pe.CloseReason = domain.CloseBaselineLimitReached
			}
			pe.Status, pe.ClosedBy, pe.ClosedAt, pe.CloseMessage = domain.PlatformExpClosed, by, &closedAt, message
		}
	}
	return nil
}

func (s *statusStore) AgentJobsByAgent(context.Context, string) (map[string]db.AgentJobs, error) {
	return map[string]db.AgentJobs{}, nil
}

// RecordClose keeps the first record and answers it after the summary, as the store does.
func (s *statusStore) RecordClose(_ context.Context, id, record string) error {
	for _, pe := range s.created {
		if pe.ID == id && pe.Status == domain.PlatformExpClosed && s.records[id] == "" {
			if s.records == nil {
				s.records = map[string]string{}
			}
			s.records[id] = record
			pe.Summary = strings.TrimLeft(pe.Summary+"\n\n"+record, "\n")
		}
	}
	return nil
}

// A job that broke a constraint beat nothing, and a person or coordinator who declares chose
// among the baseline agent's jobs.
func TestOnlyAJobHoldingTheConstraintsMakesADeclarationWeak(t *testing.T) {
	pe := openExperiment()
	declared := measuredBaseline()
	broken := measuredBaseline()
	broken.Job.ID, broken.Reported = "job-broken", map[string]float64{"val_loss": 1.1, "exact_match": 0.5}
	unreported := measuredBaseline()
	unreported.Job.ID, unreported.Reported = "job-unreported", map[string]float64{"val_loss": 1.1}
	stronger := measuredBaseline()
	stronger.Job.ID, stronger.Job.Branch, stronger.Reported = "job-check", "hl/pe-1/baseline-check", map[string]float64{"val_loss": 1.5, "exact_match": 1}
	jobs := map[string]*Baseline{"job-1": declared, "job-broken": broken, "job-unreported": unreported, "job-check": stronger}
	newSvc := func(completed ...*Baseline) (*Service, *createdStore) {
		stored := *pe
		store := &createdStore{created: []*domain.PlatformExperiment{&stored}}
		for _, b := range completed {
			store.completed = append(store.completed, b.Job)
		}
		svc := NewService(store, platformexperimentstest.Settings, zap.NewNop(), nil, time.Minute)
		svc.baseline = func(_ context.Context, _ *domain.PlatformExperiment, jobID string) (*Baseline, error) {
			return jobs[jobID], nil
		}
		return svc, store
	}
	p := *pe
	p.BaselineJobID = "job-1"
	svc, _ := newSvc(declared, broken, unreported)
	if _, err := svc.heldToBaseline(context.Background(), &p, true); err != nil {
		t.Errorf("beaten only by jobs that broke or left out a constraint: %v, want declared", err)
	}
	svc, _ = newSvc(declared, broken, stronger)
	var refused *refusal.Refusal
	if _, err := svc.heldToBaseline(context.Background(), &p, true); !errors.As(err, &refused) || refused.Reason != "baseline_weaker_than_own_job" || !strings.Contains(refused.Message, "job job-check") {
		t.Errorf("beaten by the check: %v, want baseline_weaker_than_own_job naming job-check", err)
	}
	if _, err := svc.heldToBaseline(context.Background(), &p, false); err != nil {
		t.Errorf("declared by a person: %v, want declared", err)
	}
}

// summarizedStore is statusStore with the write of a summary, which fails while broken is set.
type summarizedStore struct {
	statusStore
	broken error
}

func (s *summarizedStore) SetPlatformExperimentSummary(_ context.Context, id, summary string) error {
	if s.broken != nil {
		return s.broken
	}
	for _, pe := range s.created {
		if pe.ID == id {
			if record := s.records[id]; record != "" {
				summary = strings.TrimLeft(summary+"\n\n"+record, "\n")
			}
			pe.Summary = summary
			return nil
		}
	}
	return db.ErrPlatformExperimentNotFound
}

func (s *summarizedStore) NameActors(context.Context, []*domain.Actor) error { return nil }
