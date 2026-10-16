package quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// resolvedPool is pool as it is stored: each entry's agent type and model filled as a joining
// agent's are, entries that come to name one agent type and model merged. 422 invalid_agents for
// an entry no agent can be started as; validate holds the rest, max_agents among it.
func (h *PlatformExperimentsHandler) resolvedPool(catalog domain.AgentTypeCatalog, pool domain.ResearcherPool) (domain.ResearcherPool, error) {
	resolved, err := pool.Resolved(catalog)
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
// in the write that cut, so no cut researcher is replaced. Nor is one that ended early, and an entry
// is given at most domain.MaxReplacementsAnHour replacements an hour: the places it keeps empty
// are told on the platform experiment (db.HoldResearchers), and the entry is checked again in the
// write that adds each one (db.LateJoin.Pool).
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
	catalog, err := s.store.AgentTypeCatalog(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
	}
	pool, err := state.Pool.Resolved(catalog)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepResearchers: the researchers of %s: %w", pe.ID, err)
	}
	ended := make(map[domain.PoolKey][]domain.EndedPlace, len(state.Ended))
	predecessors := map[string]db.EndedResearcher{}
	for key, researchers := range state.Ended {
		for _, r := range researchers {
			ended[key] = append(ended[key], domain.EndedPlace{AgentID: r.AgentID, Early: r.Early})
			predecessors[r.AgentID] = r
		}
	}
	places, holds := domain.PoolDeficit(pool, state.MaxAgents, state.AtWork, ended, state.Recent)
	if line := domain.HoldsLine(holds); line != state.Held {
		if err := s.store.HoldResearchers(ctx, pe.ID, line); err != nil {
			return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
		}
	}
	if len(places) == 0 {
		return nil
	}
	refused, err := s.refusedModels(ctx, pe, catalog, now)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
	}
	if err := s.requireHosting(ctx, launch.Launcher); err != nil {
		return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
	}
	// Nobody's credential adds it: it is the platform's, on behalf of whoever created the run.
	var by domain.Actor
	if pe.Actor != nil {
		by = *pe.Actor
	}
	for _, place := range places {
		if refused[domain.ResolveModel(catalog, place.Entry.AgentType, place.Entry.Model, launch.DefaultModel)] {
			continue
		}
		hyperparameters, tier := place.Entry.Hyperparameters, domain.QuotaTier("")
		if predecessor, ok := predecessors[place.Replaces]; ok {
			hyperparameters, tier = predecessor.Hyperparameters, predecessor.QuotaTier
		}
		choice, err := domain.ResolveAgentLaunch(catalog, domain.AgentRoleResearcher, place.Entry.AgentType, place.Entry.Model, hyperparameters)
		if err != nil {
			s.logger.Warn("keep researchers: an entry no researcher can be started as", zap.String("platform_experiment_id", pe.ID), zap.String("agent_type", place.Entry.AgentType), zap.Error(err))
			continue
		}
		signup := domain.AgentSignup{
			Role: domain.SignupRoleCompetitor, QuotaTier: tier, Hosted: true, Launcher: launch.Launcher,
			AgentType: choice.AgentType, Model: choice.Model, Hyperparameters: choice.Hyperparameters, Actor: &by, Replaces: place.Replaces,
		}
		late, err := s.lateShare(ctx, pe)
		if err != nil {
			return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
		}
		late.Pool = true
		id, err := domain.NewID(domain.AgentIDPrefix)
		if err != nil {
			return fmt.Errorf("platform_experiments.KeepResearchers: %w", err)
		}
		signup.AgentID = id
		agent := &domain.Agent{ID: id, CreatedAt: now}
		err = s.store.JoinRunning(ctx, pe.ID, agent, signup, *late)
		switch {
		case errors.Is(err, db.ErrNoShareLeft), errors.Is(err, db.ErrMaxAgentsReached), errors.Is(err, db.ErrMaxHostedReached), errors.Is(err, db.ErrSignupClosed), errors.Is(err, db.ErrPoolSatisfied):
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
// An agent on its type's model is on the model catalog resolves.
func (s *PlatformExperimentsService) refusedModels(ctx context.Context, pe *domain.PlatformExperiment, catalog domain.AgentTypeCatalog, now time.Time) (map[string]bool, error) {
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
			refused[domain.ResolveModel(catalog, signup.AgentType, signup.Model, s.models.Agent)] = true
		}
	}
	return refused, nil
}
