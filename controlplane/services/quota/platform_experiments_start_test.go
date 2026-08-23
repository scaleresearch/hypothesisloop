package quota

import (
	"context"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// startStore fakes the two calls Start makes and hands the allocation callback a fixed signup
// set, so what is under test is the allocation itself rather than the transaction around it.
// Every other method is inherited from the embedded nil interface and panics if reached.
type startStore struct {
	PlatformExperimentsStore
	pe           *domain.PlatformExperiment
	participants []db.StartParticipant
}

func (f *startStore) GetPlatformExperiment(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	return f.pe, nil
}

func (f *startStore) StartPlatformExperimentTx(ctx context.Context, id string,
	quotasFor func([]db.StartParticipant) ([]*domain.AgentQuota, error)) (bool, []*domain.AgentQuota, error) {
	quotas, err := quotasFor(f.participants)
	if err != nil {
		return false, nil, err
	}
	return true, quotas, nil
}

func startWith(t *testing.T, participants ...db.StartParticipant) map[string]*domain.AgentQuota {
	t.Helper()
	store := &startStore{
		pe: &domain.PlatformExperiment{
			ID: "pe-1", Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 100,
			Stages: []domain.Stage{{LengthPct: 50, EvictPct: 50}, {LengthPct: 50, EvictPct: 0}},
		},
		participants: participants,
	}
	svc := newSignupTestService(t, store)
	svc.cfg = domain.QuotaConfig{BurstFraction: 0.5}

	quotas, err := svc.Start(context.Background(), "pe-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	byAgent := make(map[string]*domain.AgentQuota, len(quotas))
	for _, q := range quotas {
		byAgent[q.AgentID] = q
	}
	return byAgent
}

func participant(agentID string, kind domain.AgentKind, override domain.QuotaTier) db.StartParticipant {
	return db.StartParticipant{AgentID: agentID, AgentExists: true, Kind: kind, QuotaTierOverride: override}
}

// The tier is decided at signup and applied exactly once, here, when the allocation is made.
// Nothing downstream re-reads it, so an allocation that ignores it is not corrected later: the
// agent simply holds guaranteed hours — priority capacity admission honours ahead of everyone
// else's burst — for the whole run.
func TestStartAllocatesEachParticipantIntoTheTierItsKindEntitlesItTo(t *testing.T) {
	byAgent := startWith(t,
		participant("human-1", domain.AgentKindHuman, ""),
		participant("agent-1", domain.AgentKindAgent, ""),
	)

	human, agent := byAgent["human-1"], byAgent["agent-1"]
	if human.GuaranteedAcceleratorHours <= 0 {
		t.Errorf("human guaranteed = %v, want > 0", human.GuaranteedAcceleratorHours)
	}
	if agent.GuaranteedAcceleratorHours != 0 {
		t.Errorf("agent guaranteed = %v, want 0 — an autonomous participant is burst-only", agent.GuaranteedAcceleratorHours)
	}
	// The tier decides which column the share lands in, never how large it is. Taking the
	// guaranteed part away instead of moving it would quietly hand the whole budget to whichever
	// participants happened to be human.
	humanTotal := human.GuaranteedAcceleratorHours + human.BurstAcceleratorHours
	agentTotal := agent.GuaranteedAcceleratorHours + agent.BurstAcceleratorHours
	if humanTotal != agentTotal {
		t.Errorf("totals = human %v, agent %v; want equal — the tier routes a share, it does not shrink one", humanTotal, agentTotal)
	}
}

// The override is the whole point of storing a tier per signup rather than deriving it from the
// kind on read: one run can mix them however it likes.
func TestStartLetsASignupOverrideWinOverTheAgentsKind(t *testing.T) {
	byAgent := startWith(t,
		participant("agent-guaranteed", domain.AgentKindAgent, domain.QuotaTierGuaranteed),
		participant("human-burst-only", domain.AgentKindHuman, domain.QuotaTierBurstOnly),
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
			ID: "pe-1", Status: domain.PlatformExpOpen, BudgetAcceleratorHours: 100,
			Stages: []domain.Stage{{LengthPct: 100, EvictPct: 0}},
		},
		participants: []db.StartParticipant{
			participant("agent-1", domain.AgentKindAgent, ""),
			{AgentID: "ghost", AgentExists: false},
		},
	}
	if _, err := newSignupTestService(t, store).Start(context.Background(), "pe-1"); err == nil {
		t.Fatal("Start succeeded with an unresolvable signup, want an error — the run would begin with that agent holding no quota and no way to get one")
	}
}

// Only the first stage's share is released at the start; the rest is released at the boundaries
// the ladder reaches. Allocating the whole budget up front lets one participant spend it all
// before anyone has been cut.
// The participant here is a human, so its share stays where AllocateQuota put it and the released
// figure can be read off the guaranteed column directly. Burst is headroom derived from it
// (BurstFraction), not a second slice of the budget, so it is deliberately not counted.
func TestStartReleasesOnlyTheFirstStagesShareOfTheBudget(t *testing.T) {
	byAgent := startWith(t, participant("human-1", domain.AgentKindHuman, ""))

	if got, want := byAgent["human-1"].GuaranteedAcceleratorHours, 100.0*0.5; got != want {
		t.Errorf("sole participant's guaranteed = %v, want the first stage's share (%v) of a 100-hour budget — releasing more lets one agent spend the whole run's budget before anyone is cut", got, want)
	}
}
