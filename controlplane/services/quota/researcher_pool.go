package quota

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// resolvedPool is pool as it is stored: each entry's agent type and model filled as a joining
// agent's are, entries that come to name one agent type and model merged. 422 invalid_agents for
// an entry no agent can be started as; validate holds the rest, max_agents among it.
func (h *PlatformExperimentsHandler) resolvedPool(pool domain.ResearcherPool) (domain.ResearcherPool, error) {
	resolved, err := pool.Resolved(h.launch.DefaultModel)
	if err != nil {
		return nil, refuseUpdate(422, "invalid_agents", "%s", err)
	}
	return resolved, nil
}

// ResearcherKeeper keeps the researchers of running platform experiments at work, as their pools
// say (KeepResearchers), with what a hosted researcher is launched with.
type ResearcherKeeper struct {
	svc    *PlatformExperimentsService
	launch AgentLaunch
}

// NewResearcherKeeper returns the keeper of svc's platform experiments' researchers, which it
// launches as launch says.
func NewResearcherKeeper(svc *PlatformExperimentsService, launch AgentLaunch) *ResearcherKeeper {
	return &ResearcherKeeper{svc: svc, launch: launch}
}

// KeepResearchers is KeepResearchers of the keeper's service.
func (k *ResearcherKeeper) KeepResearchers(ctx context.Context, pe *domain.PlatformExperiment, now time.Time) error {
	return k.svc.KeepResearchers(ctx, pe, k.launch, now)
}

// KeepResearchers adds to the running pe, whose coordinator the platform hosts and so whose
// researchers it runs, the researchers its pool wants beyond those at work (domain.PoolDeficit):
// each a new agent of its entry's agent type and model, which takes the place of a researcher of that
// entry that finished or whose machine stopped with no restart left, with its hyperparameters and
// quota tier, or of nobody where the pool gained a place, with the entry's. Never more than max_agents
// leaves beside every researcher at work, the coordinator's additions among them. A cut lowered the pool
// in the write that cut, so no cut researcher is replaced.
//
// It adds none while less of the run is left than domain.ReplacementLead, none of an entry whose
// model account refuses its calls now, and stops at the first one the budget its researchers
// left unspent holds no trial for (db.ErrNoShareLeft): each is given its quota as any researcher
// that joins a running platform experiment is (lateShare).
func (s *PlatformExperimentsService) KeepResearchers(ctx context.Context, pe *domain.PlatformExperiment, launch AgentLaunch, now time.Time) error {
	if pe.Status != domain.PlatformExpRunning || !pe.CoordinatorHosted {
		return nil
	}
	if pe.EndsAt.Sub(now) < domain.ReplacementLead(pe.JobEstimatedDurationHours) {
		return nil
	}
	if launch.Launcher == "" || launch.DefaultModel == "" {
		return errors.New("platform_experiments.KeepResearchers: no agent launch settings")
	}
	state, err := s.store.ResearcherPoolState(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
	}
	pool, err := state.Pool.Resolved(launch.DefaultModel)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepResearchers: the researchers of %s: %w", pe.ID, err)
	}
	ended := make(map[domain.PoolKey][]string, len(state.Ended))
	predecessors := map[string]db.EndedResearcher{}
	for key, researchers := range state.Ended {
		for _, r := range researchers {
			ended[key] = append(ended[key], r.AgentID)
			predecessors[r.AgentID] = r
		}
	}
	places := domain.PoolDeficit(pool, state.MaxAgents, state.AtWork, ended)
	if len(places) == 0 {
		return nil
	}
	refused, err := s.refusedModels(ctx, pe, now)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
	}
	if err := s.requireLauncher(ctx, launch.Launcher); err != nil {
		return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
	}
	// Nobody's credential adds it: it is the platform's, on behalf of whoever created the run.
	var by domain.Actor
	if pe.Actor != nil {
		by = *pe.Actor
	}
	for _, place := range places {
		if refused[place.Entry.Model] {
			continue
		}
		signup := domain.AgentSignup{
			Role: domain.SignupRoleCompetitor, Hosted: true, Launcher: launch.Launcher,
			Flavor: place.Entry.AgentType, Model: place.Entry.Model, Hyperparameters: place.Entry.Hyperparameters, Actor: &by, Replaces: place.Replaces,
		}
		if predecessor, ok := predecessors[place.Replaces]; ok {
			signup.Hyperparameters, signup.QuotaTier = predecessor.Hyperparameters, predecessor.QuotaTier
		}
		late, err := s.lateShare(ctx, pe)
		if err != nil {
			return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
		}
		id, err := domain.NewID(domain.AgentIDPrefix)
		if err != nil {
			return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
		}
		signup.AgentID = id
		agent := &domain.Agent{ID: id, PerformanceScore: 0.5, CreatedAt: now}
		err = s.store.JoinRunning(ctx, pe.ID, agent, signup, *late)
		switch {
		case errors.Is(err, db.ErrNoShareLeft), errors.Is(err, db.ErrMaxAgentsReached), errors.Is(err, db.ErrMaxHostedReached), errors.Is(err, db.ErrSignupClosed):
			s.logger.Info("keep researchers: none added", zap.String("platform_experiment_id", pe.ID), zap.String("replaces", place.Replaces), zap.String("why", err.Error()))
			return nil
		case err != nil:
			return fmt.Errorf("platform_experiments.KeepResearchers: add a researcher of %s on %s: %w", place.Entry.AgentType, place.Entry.Model, err)
		}
		s.logger.Info("researcher added to keep the pool", zap.String("platform_experiment_id", pe.ID), zap.String("agent_id", id),
			zap.String("replaces", place.Replaces), zap.String("agent_type", place.Entry.AgentType), zap.String("model", place.Entry.Model))
	}
	return nil
}

// refusedModels is the models whose account the provider refuses the calls of now, as the
// harness of an agent of pe at work on one last read it: a researcher added on one would only wait.
func (s *PlatformExperimentsService) refusedModels(ctx context.Context, pe *domain.PlatformExperiment, now time.Time) (map[string]bool, error) {
	states, err := metricsdb.LatestHarnessState(ctx, s.metricsDBURL, pe.ID, pe.CreatedAt)
	if err != nil {
		return nil, err
	}
	if len(states) == 0 {
		return nil, nil
	}
	signups, err := s.store.ListAgentSignups(ctx, pe.ID)
	if err != nil {
		return nil, err
	}
	byAgent := make(map[string]domain.AgentSignup, len(signups))
	for _, signup := range signups {
		byAgent[signup.AgentID] = signup
	}
	refused := map[string]bool{}
	for _, state := range states {
		signup, ok := byAgent[state.AgentID]
		if !ok || signup.Ended != "" || !liveReading(signup.Instance, state) {
			continue
		}
		if allowance := domain.ModelAllowanceOf(state.Model, now); allowance.RefusedUntil != nil {
			refused[strings.TrimSpace(signup.Model)] = true
		}
	}
	return refused, nil
}
