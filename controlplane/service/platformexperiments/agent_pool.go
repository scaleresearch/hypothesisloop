package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
)

// resolvedPool is pool as it is stored: each entry's agent type and model filled as a joining
// agent's are, entries that come to name one agent type and model merged. 422 invalid_agents for
// an entry no agent can be started as, and model_not_served for a model the platform does not
// serve; validate holds the rest, max_agents among it.
func (s *Service) resolvedPool(ctx context.Context, catalog domain.AgentTypeCatalog, pool domain.AgentPool) (domain.AgentPool, error) {
	resolved, err := pool.Resolved(catalog)
	if err != nil {
		return nil, refuseUpdate(422, "invalid_agents", "%s", err)
	}
	for i, e := range pool {
		if err := s.served.CheckChoice(ctx, fmt.Sprintf("agents[%d].model", i), e.Model); err != nil {
			return nil, s.refusal("resolve agents", err)
		}
	}
	return resolved, nil
}

// KeepAgents adds to the running pe, whose coordinator the platform hosts and so whose
// agents it runs, the agents its pool wants beyond those at work (domain.PoolDeficit):
// each a new agent of its entry's agent type and model, which takes the place of an agent of that
// entry that finished or whose machine stopped with no restart left, with its hyperparameters and
// quota tier, or of nobody where the pool gained a place, with the entry's. Never more executors than
// max_agents leaves beside every executor at work, the coordinator's additions among them. A cut lowered
// the pool in the write that cut, so no cut agent is replaced. Nor is one that ended early, and an entry
// is given at most domain.MaxReplacementsAnHour replacements an hour: the places it keeps empty
// are told on the platform experiment (db.HoldAgents), and the entry is checked again in the
// write that adds each one (db.LateJoin.Pool).
//
// It adds none while less of the run is left than domain.ReplacementLead, and none of an entry whose
// model account refuses its calls now. Once the budget its executors left unspent holds no trial
// for one (db.ErrNoShareLeft), or max_agents executors are at work, it adds no further executor
// and still fills the researchers' places: each executor is given its quota as any executor that
// joins a running platform experiment is (lateShare).
func (s *Service) KeepAgents(ctx context.Context, pe *domain.PlatformExperiment, now time.Time) error {
	launch := s.launch
	if pe.Status != domain.PlatformExpRunning || !pe.CoordinatorInAgentSandbox {
		return nil
	}
	if pe.EndsAt.Sub(now) < domain.ReplacementLead(pe.JobEstimatedDurationHours) {
		return nil
	}
	if launch.DefaultModel == "" {
		return errors.New("platform_experiments.KeepAgents: no agent launch settings")
	}
	state, err := s.store.AgentPoolState(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepAgents: %w", err)
	}
	catalog, err := s.store.AgentTypeCatalog(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepAgents: %w", err)
	}
	pool, err := state.Pool.Resolved(catalog)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepAgents: the agents of %s: %w", pe.ID, err)
	}
	ended := make(map[domain.PoolKey][]domain.EndedPlace, len(state.Ended))
	predecessors := map[string]db.EndedAgent{}
	for key, agents := range state.Ended {
		for _, r := range agents {
			ended[key] = append(ended[key], domain.EndedPlace{AgentID: r.AgentID, Early: r.Early})
			predecessors[r.AgentID] = r
		}
	}
	places, holds := domain.PoolDeficit(pool, state.MaxAgents, state.AtWork, ended, state.Recent)
	if line := domain.HoldsLine(holds); line != state.Held {
		if err := s.store.HoldAgents(ctx, pe.ID, line); err != nil {
			return fmt.Errorf("platform_experiments.KeepAgents: %w", err)
		}
	}
	if len(places) == 0 {
		return nil
	}
	refused, err := s.refusedModels(ctx, pe, catalog, now)
	if err != nil {
		return fmt.Errorf("platform_experiments.KeepAgents: %w", err)
	}
	if err := requireAgentSandbox(launch.AgentSandbox); err != nil {
		return fmt.Errorf("platform_experiments.KeepAgents: %w", err)
	}
	// Nobody's credential adds it: it is the platform's, on behalf of whoever created the run.
	var by domain.Actor
	if pe.Actor != nil {
		by = *pe.Actor
	}
	executorsFull := false
	// What the executors used, read once for the pass: a newcomer has used nothing.
	var share *db.LateJoin
	for _, place := range places {
		executor := !place.Entry.Key().Researches()
		if executor && executorsFull || refused[s.poolModel(pe, catalog, place.Entry)] {
			continue
		}
		hyperparameters, tier := place.Entry.Hyperparameters, domain.QuotaTier("")
		if predecessor, ok := predecessors[place.Replaces]; ok {
			hyperparameters, tier = predecessor.Hyperparameters, predecessor.QuotaTier
		}
		choice, err := domain.ResolveAgentLaunch(catalog, domain.PoolRole(place.Entry.AgentType), place.Entry.AgentType, place.Entry.Model, hyperparameters)
		if err != nil {
			s.logger.Warn("keep agents: an entry no agent can be started as", zap.String("platform_experiment_id", pe.ID), zap.String("agent_type", place.Entry.AgentType), zap.Error(err))
			continue
		}
		signup := domain.AgentSignup{
			Role: domain.PoolRole(choice.AgentType).Info().SignupRole, QuotaTier: tier, InAgentSandbox: true,
			AgentType: choice.AgentType, Model: choice.Model, Hyperparameters: choice.Hyperparameters, Actor: &by, Replaces: place.Replaces,
		}
		late := db.LateJoin{Pool: true}
		if executor {
			if share == nil {
				if share, err = s.lateShare(ctx, pe); err != nil {
					s.logger.Error("keep agents: the share of a late executor, so no executor is added", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
					executorsFull = true
					continue
				}
				share.Pool = true
			}
			late = *share
		}
		id, err := domain.NewID(domain.AgentIDPrefix)
		if err != nil {
			s.logger.Error("keep agents: an agent id", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			continue
		}
		signup.AgentID = id
		agent := &domain.Agent{ID: id, CreatedAt: now}
		err = s.store.JoinRunning(ctx, pe.ID, agent, signup, late)
		switch {
		case errors.Is(err, db.ErrNoShareLeft), errors.Is(err, db.ErrMaxAgentsReached):
			s.logger.Info("keep agents: no further executor added", zap.String("platform_experiment_id", pe.ID), zap.String("replaces", place.Replaces), zap.String("why", err.Error()))
			executorsFull = true
			continue
		case errors.Is(err, db.ErrPoolSatisfied):
			s.logger.Info("keep agents: none added for this entry", zap.String("platform_experiment_id", pe.ID), zap.String("replaces", place.Replaces), zap.String("why", err.Error()))
			continue
		case errors.Is(err, db.ErrMaxAgentSandboxesReached), errors.Is(err, db.ErrSignupClosed):
			s.logger.Info("keep agents: none added", zap.String("platform_experiment_id", pe.ID), zap.String("replaces", place.Replaces), zap.String("why", err.Error()))
			return nil
		case err != nil:
			s.logger.Error("keep agents: add an agent failed", zap.String("platform_experiment_id", pe.ID), zap.String("replaces", place.Replaces),
				zap.String("agent_type", place.Entry.AgentType), zap.String("model", place.Entry.Model), zap.Error(err))
			continue
		}
		s.logger.Info("agent added to keep the pool", zap.String("platform_experiment_id", pe.ID), zap.String("agent_id", id),
			zap.String("replaces", place.Replaces), zap.String("agent_type", place.Entry.AgentType), zap.String("model", place.Entry.Model))
	}
	return nil
}

// poolModel is the model an agent of entry of pe runs on, as its instructions resolve it.
func (s *Service) poolModel(pe *domain.PlatformExperiment, catalog domain.AgentTypeCatalog, entry domain.AgentPoolEntry) string {
	choice := domain.ModelChoice{Role: domain.PoolRole(entry.AgentType), Own: entry.Model, Type: domain.TypeModel(catalog, entry.AgentType)}
	if pe.Coordinator != nil {
		choice.Coordinator = pe.Coordinator.Model
	}
	return choice.Model(s.models)
}

// refusedModels is the models whose account the provider refuses the calls of now, as the
// harness of an agent of pe at work on one last read it: an agent added on one would only wait.
// An agent on its type's model is on the model catalog resolves.
func (s *Service) refusedModels(ctx context.Context, pe *domain.PlatformExperiment, catalog domain.AgentTypeCatalog, now time.Time) (map[string]bool, error) {
	states, err := s.metrics.LatestHarnessState(ctx, pe.ID, pe.CreatedAt)
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
			refused[s.poolModel(pe, catalog, domain.AgentPoolEntry{AgentType: signup.AgentType, Model: signup.Model})] = true
		}
	}
	return refused, nil
}
