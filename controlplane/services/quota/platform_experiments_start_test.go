package quota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// startStore fakes the calls a start makes and hands the allocation callback a fixed signup
// set, so what is under test is the allocation itself rather than the transaction around it.
// Every other method is inherited from the embedded nil interface and panics if reached.
type startStore struct {
	fakeStore
	pe           *domain.PlatformExperiment
	participants []db.StartParticipant
	// baselines are the agents of role baseline, which take no share and whose spend is taken
	// from the budget before the shares are carved.
	baselines []string
	// stages is the ladder the start stored.
	stages []domain.Stage
}

func (f *startStore) ListSignupsByRole(_ context.Context, _ string, role domain.SignupRole) ([]string, error) {
	if role != domain.SignupRoleBaseline {
		return nil, fmt.Errorf("asked for the agents of role %s", role)
	}
	return f.baselines, nil
}

func (f *startStore) ListAgentSignups(context.Context, string) ([]domain.AgentSignup, error) {
	signups := []domain.AgentSignup{{AgentID: "ag-planner", Role: domain.SignupRolePlanner}}
	for _, agentID := range f.baselines {
		signups = append(signups, domain.AgentSignup{AgentID: agentID, Role: domain.SignupRoleBaseline})
	}
	for _, p := range f.participants {
		signups = append(signups, domain.AgentSignup{AgentID: p.AgentID, Role: domain.SignupRoleCompetitor})
	}
	return signups, nil
}

func (f *startStore) AgentJobsByAgent(context.Context, string) (map[string]db.AgentJobs, error) {
	return map[string]db.AgentJobs{}, nil
}

func (f *startStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	return f.pe, nil
}

func (f *startStore) StartPlatformExperimentTx(ctx context.Context, id string, _ time.Duration,
	quotasFor func([]db.StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error)) (bool, []*domain.AgentQuota, error) {
	participants := slices.Clone(f.participants)
	for i := range participants {
		if participants[i].Role == "" {
			participants[i].Role = domain.SignupRoleCompetitor
		}
	}
	quotas, stages, err := quotasFor(participants)
	if err != nil {
		return false, nil, err
	}
	f.stages = stages
	return true, quotas, nil
}

func startWith(t *testing.T, participants ...db.StartParticipant) map[string]*domain.AgentQuota {
	return startWithDefault(t, domain.QuotaTierGuaranteed, participants...)
}

func startWithDefault(t *testing.T, defaultTier domain.QuotaTier, participants ...db.StartParticipant) map[string]*domain.AgentQuota {
	t.Helper()
	store := &startStore{
		pe: &domain.PlatformExperiment{
			ID: "pe-1", Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 100, BaselineJobID: "job-baseline",
			Stages:           []domain.Stage{{LengthPct: 50, EvictPct: 50}, {LengthPct: 50, EvictPct: 0}},
			DefaultQuotaTier: defaultTier,
		},
		participants: participants,
	}
	svc := newSignupTestService(t, store)
	svc.cfg = domain.QuotaConfig{BurstFraction: 0.5}

	quotas, err := svc.start(context.Background(), store.pe, time.Now().UTC())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	byAgent := make(map[string]*domain.AgentQuota, len(quotas))
	for _, q := range quotas {
		byAgent[q.AgentID] = q
	}
	return byAgent
}

// participant is a signup of agentID whose tier is tier, guaranteed where it states none, as the
// default of the experiments here resolves it at signup.
func participant(agentID string, tier domain.QuotaTier) db.StartParticipant {
	if tier == "" {
		tier = domain.QuotaTierGuaranteed
	}
	return db.StartParticipant{AgentID: agentID, AgentExists: true, QuotaTier: tier}
}

// The tier is decided at signup and applied exactly once, here, when the allocation is made.
// Nothing downstream re-reads it, so an allocation that ignores it is not corrected later: the
// agent simply holds guaranteed hours — priority capacity admission honours ahead of everyone
// else's burst — for the whole run.
func TestStartGuaranteedDefaultAllocatesEqualGuaranteedSharesRegardlessOfKind(t *testing.T) {
	byAgent := startWith(t,
		participant("human-1", ""),
		participant("agent-1", ""),
	)

	human, agent := byAgent["human-1"], byAgent["agent-1"]
	if human.GuaranteedAcceleratorHours <= 0 {
		t.Errorf("human guaranteed = %v, want > 0", human.GuaranteedAcceleratorHours)
	}
	if agent.GuaranteedAcceleratorHours <= 0 {
		t.Errorf("agent guaranteed = %v, want > 0 under the experiment's guaranteed default", agent.GuaranteedAcceleratorHours)
	}
	// The tier decides which column the share lands in, never how large it is. Taking the
	// guaranteed part away instead of moving it would quietly hand the whole budget to whichever
	// participants happened to be human.
	humanTotal := human.GuaranteedAcceleratorHours + human.BurstAcceleratorHours
	agentTotal := agent.GuaranteedAcceleratorHours + agent.BurstAcceleratorHours
	if humanTotal != agentTotal {
		t.Errorf("totals = human %v, agent %v; want equal — the tier routes a share, it does not shrink one", humanTotal, agentTotal)
	}
	if human.GuaranteedAcceleratorHours != agent.GuaranteedAcceleratorHours {
		t.Errorf("guaranteed shares = human %v, agent %v; want equal", human.GuaranteedAcceleratorHours, agent.GuaranteedAcceleratorHours)
	}
}

// The override is the whole point of storing a tier per signup rather than deriving it from the
// kind on read: one run can mix them however it likes.
func TestStartLetsASignupOverrideWinOverTheAgentsKind(t *testing.T) {
	byAgent := startWith(t,
		participant("agent-guaranteed", domain.QuotaTierGuaranteed),
		participant("human-burst-only", domain.QuotaTierBurstOnly),
	)

	if g := byAgent["agent-guaranteed"].GuaranteedAcceleratorHours; g <= 0 {
		t.Errorf("agent granted guaranteed: guaranteed = %v, want > 0 despite its kind", g)
	}
	if g := byAgent["human-burst-only"].GuaranteedAcceleratorHours; g != 0 {
		t.Errorf("human restricted to burst_only: guaranteed = %v, want 0 despite its kind", g)
	}
	if b := byAgent["human-burst-only"].BurstAcceleratorHours; b <= 0 {
		t.Errorf("human restricted to burst_only: burst = %v, want > 0 — the share moves, it is not forfeited", b)
	}
}

// A signup naming an agent that does not exist fails the whole start. Skipping it used to mean a
// transient read permanently disinherited a participant: the run started without it, and there is
// no second allocation pass.
func TestStartFailsRatherThanStartingWithoutAParticipantItCouldNotResolve(t *testing.T) {
	store := &startStore{
		pe: &domain.PlatformExperiment{
			ID: "pe-1", Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 100, BaselineJobID: "job-baseline",
			Stages: []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		},
		participants: []db.StartParticipant{
			participant("agent-1", ""),
			{AgentID: "ghost", AgentExists: false},
		},
	}
	if _, err := newSignupTestService(t, store).start(context.Background(), store.pe, time.Now().UTC()); err == nil {
		t.Fatal("Start succeeded with an unresolvable signup, want an error — the run would begin with that agent holding no quota and no way to get one")
	}
}

// reasonOf is the reason code of a refusal, empty for anything else.
func reasonOf(err error) string {
	var refused *refusal.Refusal
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}

// A platform experiment started on a baseline nobody had measured, or on none: its researchers
// were ranked against a number each of them invented. The platform starts it only with one
// declared, and until then it waits and says so.
func TestAPlatformExperimentDoesNotStartWithoutADeclaredBaseline(t *testing.T) {
	store := &sweptStore{startStore: startStore{participants: []db.StartParticipant{participant("agent-1", "")}, pe: &domain.PlatformExperiment{
		ID: "pe-1", Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 100, StartsAt: time.Now().Add(-time.Minute),
		Stages: []domain.Stage{{LengthPct: 100, EvictPct: 0}},
	}}}
	svc := newSignupTestService(t, store)
	ctx := context.Background()

	if err := svc.SweepAutoStart(ctx); err != nil || store.started != 0 {
		t.Fatalf("the sweep past starts_at without a baseline: %v, %d starts, want it left to wait", err, store.started)
	}
	if err := svc.Describe(ctx, store.pe, auth.Principal{Kind: auth.KindUser}); err != nil || store.pe.StatusReason == nil || store.pe.StatusReason.Reason != "baseline_required" {
		t.Fatalf("an open experiment without a baseline: status_reason = %+v, %v, want baseline_required", store.pe.StatusReason, err)
	}

	store.pe.BaselineJobID = "job-baseline"
	if err := svc.Describe(ctx, store.pe, auth.Principal{Kind: auth.KindUser}); err != nil || store.pe.StatusReason.Reason != "starting" {
		t.Fatalf("with its baseline declared: status_reason = %+v, %v, want starting", store.pe.StatusReason, err)
	}
	if err := svc.SweepAutoStart(ctx); err != nil || store.started != 1 {
		t.Fatalf("the sweep once a baseline is declared: %v, %d starts, want it started", err, store.started)
	}
}

// An experiment that stated no starts_at once waited for a call nothing announced: a direct API
// user polled for baseline_job_id and its coordinator wrote that the run was proceeding. The
// platform starts every run, at starts_at or at once without one, and what stops it is read on
// the experiment.
func TestThePlatformStartsARunOnceItsBaselineIsDeclaredAndSaysWhatItWaitsFor(t *testing.T) {
	declared := func() *sweptStore {
		return &sweptStore{startStore: startStore{participants: []db.StartParticipant{participant("agent-1", "")}, pe: &domain.PlatformExperiment{
			ID: "pe-1", Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 100, BaselineJobID: "job-baseline",
			Stages: []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		}}}
	}
	ctx := context.Background()

	store := declared()
	if err := newSignupTestService(t, store).SweepAutoStart(ctx); err != nil || store.started != 1 {
		t.Fatalf("the sweep with a baseline and no starts_at: %v, %d starts, want it started", err, store.started)
	}

	for name, c := range map[string]struct {
		change func(*sweptStore)
		reason string
	}{
		"a starts_at still ahead":      {func(f *sweptStore) { f.pe.StartsAt = time.Now().Add(time.Hour) }, "starts_at_ahead"},
		"no researcher signed up":      {func(f *sweptStore) { f.participants, f.baselines = nil, []string{"ag-baseline"} }, "no_researchers"},
		"a researcher lost to a start": {func(f *sweptStore) { f.lost = true }, ""},
	} {
		store := declared()
		c.change(store)
		svc := newSignupTestService(t, store)
		if err := svc.SweepAutoStart(ctx); err != nil {
			t.Fatalf("%s: the sweep: got = %v, want it to pass over the experiment", name, err)
		}
		if _, err := svc.start(ctx, store.pe, time.Now().UTC()); err == nil || (c.reason != "" && reasonOf(err) != c.reason) {
			t.Errorf("%s: start: got = %v, want refused %s", name, err, c.reason)
		}
		if c.reason == "" {
			continue
		}
		if store.started != 0 {
			t.Errorf("%s: %d starts reached the store, want none", name, store.started)
		}
		if err := svc.Describe(ctx, store.pe, auth.Principal{Kind: auth.KindUser}); err != nil || store.pe.StatusReason == nil || store.pe.StatusReason.Reason != c.reason {
			t.Errorf("%s: status_reason = %+v, %v, want %s", name, store.pe.StatusReason, err, c.reason)
		}
	}
}

// A closed platform experiment answered close_reason and no status_reason, and a running one
// neither: "why is it in this state" is one field in every status. A run that waits for its first
// researcher's machine says so and when the wait ends.
func TestEveryStatusSaysWhyInItsStatusReason(t *testing.T) {
	svc := newSignupTestService(t, &startStore{participants: []db.StartParticipant{{AgentID: "ag-r", QuotaTier: domain.QuotaTierGuaranteed}}})
	now := time.Now().UTC()
	waiting := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning}
	waiting.SetClosesAt(now.Add(20 * time.Minute))
	started := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpRunning, StartedAt: now}
	started.SetClosesAt(now.Add(time.Hour))
	if !waiting.AwaitsAgents() || waiting.AgentsDueAt.IsZero() || !waiting.EndsAt.IsZero() || !waiting.ClosesAt().Equal(waiting.AgentsDueAt) {
		t.Fatalf("a running one with no clock: agents due %v, ends %v, closes %v, want only its agents deadline", waiting.AgentsDueAt, waiting.EndsAt, waiting.ClosesAt())
	}
	if started.AwaitsAgents() || !started.AgentsDueAt.IsZero() || !started.ClosesAt().Equal(started.EndsAt) {
		t.Fatalf("a running one whose clock runs: agents due %v, ends %v, want only its end", started.AgentsDueAt, started.EndsAt)
	}
	cases := map[string]*domain.PlatformExperiment{
		"agents_starting": waiting,
		"running":         started,
		"closed":          {ID: "pe-1", Status: domain.PlatformExpClosed},
	}
	for _, reason := range []domain.CloseReason{domain.CloseEndTimeReached, domain.CloseByRequest, domain.ClosePlanningLimitReached,
		domain.CloseProposalExpired, domain.CloseBaselineLimitReached, domain.CloseAgentsNotStarted, domain.ClosePlannerStopped, domain.CloseRepositoryNotCreated} {
		cases[string(reason)] = &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpClosed, CloseReason: reason}
	}
	for want, pe := range cases {
		if err := svc.Describe(context.Background(), pe, auth.Principal{Kind: auth.KindUser}); err != nil || pe.StatusReason == nil || pe.StatusReason.Reason != want || pe.StatusReason.Message == "" {
			t.Errorf("%s %s: status_reason = %+v, %v, want reason %s and a sentence", pe.Status, pe.CloseReason, pe.StatusReason, err, want)
			continue
		}
		// A closed one waits for nobody, and a running one for nobody but the platform.
		who := domain.WaitsForPlatform
		if pe.Status == domain.PlatformExpClosed {
			who = ""
		}
		if pe.StatusReason.WaitsFor != who {
			t.Errorf("%s %s: waits_for = %q, want %q", pe.Status, pe.CloseReason, pe.StatusReason.WaitsFor, who)
		}
	}
	// A draft waits for its planner until it answers, and from then for a person. An open one
	// whose coordinator is a person asks that person for what a hosted coordinator would do.
	answered := now
	proposed := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpDraft, ProposedAt: &answered}
	for name, c := range map[string]struct {
		pe  *domain.PlatformExperiment
		who domain.Waiter
	}{
		"a draft being planned":             {&domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpDraft}, domain.WaitsForPlatform},
		"a draft with a proposal":           {proposed, domain.WaitsForOwner},
		"an open one a person coordinates":  {&domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen}, domain.WaitsForOwner},
		"an open one with its coordinator":  {&domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen, CoordinatorHosted: true}, domain.WaitsForCoordinator},
		"an open one with nobody to run it": {&domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpOpen, CoordinatorHosted: true, BaselineJobID: "job-1"}, domain.WaitsForOwner},
	} {
		if err := svc.Describe(context.Background(), c.pe, auth.Principal{Kind: auth.KindUser}); err != nil || c.pe.StatusReason.WaitsFor != c.who {
			t.Errorf("%s: status_reason = %+v, %v, want it to wait for %s", name, c.pe.StatusReason, err, c.who)
		}
	}
	closedWaiting := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpClosed, CloseReason: domain.CloseAgentsNotStarted}
	closedWaiting.SetClosesAt(now)
	if closedWaiting.AgentsDueAt.IsZero() || !closedWaiting.BaselineDueAt.IsZero() {
		t.Errorf("closed with no machine up: agents due %v, baseline due %v, want the deadline it missed kept under its name", closedWaiting.AgentsDueAt, closedWaiting.BaselineDueAt)
	}
}

// sweptStore is startStore with what the sweep reads, counting the starts that got through.
type sweptStore struct {
	startStore
	started int
	// lost has the start find the experiment no longer open.
	lost bool
}

func (f *sweptStore) ListPlatformExperimentsByStatus(context.Context, domain.PlatformExperimentStatus) ([]*domain.PlatformExperiment, error) {
	return []*domain.PlatformExperiment{f.pe}, nil
}

func (f *sweptStore) StartPlatformExperimentTx(ctx context.Context, id string, agentsDue time.Duration, quotasFor func([]db.StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error)) (bool, []*domain.AgentQuota, error) {
	if f.lost {
		return false, nil, nil
	}
	f.started++
	return f.startStore.StartPlatformExperimentTx(ctx, id, agentsDue, quotasFor)
}

// A field a boundary can cut from is started on the ladder: only the first stage's share is
// released, and the rest at the boundaries, or one researcher spends the budget before anyone is
// cut. A smaller field is cut by no boundary, where the ladder only withheld budget: two
// researchers stopped on a first share minutes before the boundary released the rest. It has one
// stage and each researcher its whole share from the start.
// The participants here are human, so each share stays where AllocateQuota put it and is read
// off the guaranteed column. Burst is headroom derived from it, not a second slice of the budget.
func TestStartReleasesTheWholeShareUnlessABoundaryCanCut(t *testing.T) {
	ladder := []domain.Stage{{LengthPct: 50, EvictPct: 50, MaxJobHours: 1}, {LengthPct: 50, EvictPct: 0, MaxJobHours: 4}}
	field := func(researchers int, others ...db.StartParticipant) *startStore {
		store := &startStore{participants: others, pe: &domain.PlatformExperiment{
			ID: "pe-1", Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 100, BaselineJobID: "job-baseline", Stages: slices.Clone(ladder),
		}}
		for i := range researchers {
			store.participants = append(store.participants, participant(fmt.Sprintf("human-%d", i+1), ""))
		}
		return store
	}
	reviewer := participant("reviewer-1", "")
	reviewer.Role = domain.SignupRoleReviewer
	for name, c := range map[string]struct {
		store  *startStore
		share  float64
		stages []domain.Stage
	}{
		"five researchers":             {field(domain.MinResearchersForCut), 10, ladder},
		"one researcher":               {field(1), 100, []domain.Stage{{LengthPct: 100, MaxJobHours: 4}}},
		"four researchers, a reviewer": {field(4, reviewer), 20, []domain.Stage{{LengthPct: 100, MaxJobHours: 4}}},
	} {
		quotas, err := newSignupTestService(t, c.store).start(context.Background(), c.store.pe, time.Now().UTC())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, q := range quotas {
			if q.GuaranteedAcceleratorHours != c.share {
				t.Errorf("%s: %s is given %v, want %v", name, q.AgentID, q.GuaranteedAcceleratorHours, c.share)
			}
		}
		if !slices.Equal(c.store.stages, c.stages) {
			t.Errorf("%s: the run is stored with stages %+v, want %+v", name, c.store.stages, c.stages)
		}
	}

	// What measuring the baseline cost counts toward the budget: the shares are carved from what
	// it left, and a budget the baseline spent whole starts nothing.
	for spent, want := range map[float64]float64{20: 80, 100: 0} {
		metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"resource_type":"accelerator_hours","tier":"guaranteed"},"value":[1,"%v"]}]}}`, spent)
		}))
		store := &startStore{baselines: []string{"baseline-1"}, participants: []db.StartParticipant{participant("human-1", "")}, pe: &domain.PlatformExperiment{
			ID: "pe-1", Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 100, BaselineJobID: "job-baseline", CreatedAt: time.Now().Add(-time.Hour),
			Stages: []domain.Stage{{LengthPct: 50, EvictPct: 50}, {LengthPct: 50, EvictPct: 0}},
		}}
		svc := newSignupTestService(t, store)
		svc.metricsDBURL = metrics.URL
		quotas, err := svc.start(context.Background(), store.pe, time.Now().UTC())
		metrics.Close()
		if want == 0 {
			if reasonOf(err) != "budget_spent" {
				t.Errorf("start after the baseline spent %v of 100: err = %v, want it refused as spent", spent, err)
			}
			continue
		}
		if err != nil || len(quotas) != 1 || quotas[0].GuaranteedAcceleratorHours != want {
			t.Errorf("start after the baseline spent %v of 100: quotas = %+v, %v, want a share of %v", spent, quotas, err, want)
		}
	}
}

// A closed platform experiment read consumed_accelerator_hours null: nothing said what it spent.
func TestAPlatformExperimentAnswersWhatItSpent(t *testing.T) {
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"0.75"]}]}}`)
	}))
	defer metrics.Close()
	svc := NewPlatformExperimentsService(&startStore{}, testSettings, zap.NewNop(), metrics.URL, time.Minute)
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning, domain.PlatformExpClosed} {
		pe := &domain.PlatformExperiment{ID: "pe-1", Status: status, BudgetAcceleratorHours: 2, CreatedAt: time.Now().Add(-time.Hour)}
		if err := svc.Spend(context.Background(), pe); err != nil || pe.ConsumedAcceleratorHours == nil || *pe.ConsumedAcceleratorHours != 0.75 {
			t.Errorf("%s: consumed_accelerator_hours = %v, %v, want the 0.75 its jobs were billed", status, pe.ConsumedAcceleratorHours, err)
		}
	}
	draft := &domain.PlatformExperiment{ID: "pe-1", Status: domain.PlatformExpDraft}
	if err := svc.Spend(context.Background(), draft); err != nil || draft.ConsumedAcceleratorHours != nil {
		t.Errorf("a draft: consumed_accelerator_hours = %v, %v, want none: it runs no job", draft.ConsumedAcceleratorHours, err)
	}
}
