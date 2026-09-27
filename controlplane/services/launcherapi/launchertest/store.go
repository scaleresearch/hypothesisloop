package launchertest

import (
	"context"
	"sync"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Store is the launcher handler's store in memory, with the conditions the database's own
// statements apply, for tests that follow a hosted machine over several reconciles: the
// handler's, and the launcher's against the handler.
type Store struct {
	mu sync.Mutex
	// ByLauncher holds each launcher's hosted agents, Instance set on every one.
	ByLauncher   map[string][]db.LauncherAgent
	Coordinators []db.LauncherCoordinator
	// Err fails the listing, and WriteErr every write for the agent id it names.
	Err      error
	WriteErr map[string]error
	// Listed are the launchers the agents were listed for, and WindDown the last one asked with.
	Listed   []string
	WindDown time.Duration
	// Over holds, by platform experiment and agent id joined with a slash, the agents whose run
	// ended longer ago than the wind-down, and why: the launcher is no longer asked to keep
	// them, and the next question releases their machines.
	Over map[string]domain.RunEnd

	// ProgressWrites counts the times a progress marker was written down.
	ProgressWrites int
	// Holds is what each launcher's last question left recorded of it holding machines.
	Holds map[string]bool

	clock    time.Duration
	askedAt  map[string]time.Duration
	progress map[string]string
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Advance moves the store's clock, which stands in for the database's.
func (s *Store) Advance(by time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock += by
}

func (s *Store) ListLauncherAgents(_ context.Context, launcher string, windDown time.Duration) ([]db.LauncherAgent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Listed = append(s.Listed, launcher)
	s.WindDown = windDown
	listed := []db.LauncherAgent{}
	for _, agent := range s.ByLauncher[launcher] {
		if _, over := s.Over[agent.PlatformExperimentID+"/"+agent.AgentID]; over {
			continue
		}
		if agent.Instance != nil {
			instance := *agent.Instance
			agent.Instance = &instance
		}
		listed = append(listed, agent)
	}
	return listed, s.Err
}

func (s *Store) ListLauncherCoordinators(_ context.Context, launcher string, _ time.Duration) ([]db.LauncherCoordinator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	listed := []db.LauncherCoordinator{}
	for _, coordinator := range s.Coordinators {
		if coordinator.Launcher == launcher {
			listed = append(listed, coordinator)
		}
	}
	return listed, s.Err
}

// ReleaseInstances records the machines of the agents in Over as released by the platform.
func (s *Store) ReleaseInstances(_ context.Context, launcher string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return s.Err
	}
	now := epoch.Add(s.clock)
	for _, agent := range s.ByLauncher[launcher] {
		end, over := s.Over[agent.PlatformExperimentID+"/"+agent.AgentID]
		if !over || agent.Instance == nil || agent.Instance.Name == "" {
			continue
		}
		agent.Instance.Name, agent.Instance.Reported, agent.Instance.ExitedAt = "", "", &now
		agent.Instance.EndReason, agent.Instance.ExitLog = string(db.InstanceReleased), end.ReleasedBecause()
	}
	return nil
}

// Find is the stored instance of an agent, or with no agent id of the coordinator.
func (s *Store) Find(platformExpID, agentID string) *domain.AgentInstance {
	if agentID == "" {
		for i := range s.Coordinators {
			if s.Coordinators[i].PlatformExperimentID == platformExpID {
				return &s.Coordinators[i].Instance
			}
		}
	}
	for _, agents := range s.ByLauncher {
		for i := range agents {
			if agents[i].PlatformExperimentID == platformExpID && agents[i].AgentID == agentID {
				return agents[i].Instance
			}
		}
	}
	panic("launchertest: nothing hosted as " + platformExpID + "/" + agentID)
}

func (s *Store) AskInstance(_ context.Context, platformExpID, agentID, name, launch string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.WriteErr[agentID]; err != nil {
		return err
	}
	instance := s.Find(platformExpID, agentID)
	if instance.Launch != launch {
		instance.Restarts, instance.Stopped = 0, false
	}
	instance.Name, instance.Launch, instance.SeenAt, instance.Reported, instance.ProgressAt = name, launch, nil, "", nil
	delete(s.progress, platformExpID+"/"+agentID)
	if s.askedAt == nil {
		s.askedAt = map[string]time.Duration{}
	}
	s.askedAt[platformExpID+"/"+agentID] = s.clock
	return nil
}

func (s *Store) ObserveInstance(_ context.Context, platformExpID, agentID, name, state string, progress *string, progressLimit time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.WriteErr[agentID]; err != nil {
		return err
	}
	instance := s.Find(platformExpID, agentID)
	if name == "" || instance.Name != name {
		return nil
	}
	at := epoch.Add(s.clock)
	if instance.SeenAt == nil {
		instance.SeenAt = &at
	}
	instance.Reported = state
	key := platformExpID + "/" + agentID
	if progress != nil && *progress != s.progress[key] && (instance.ProgressAt == nil || at.Sub(*instance.ProgressAt) >= progressLimit/4) {
		if s.progress == nil {
			s.progress = map[string]string{}
		}
		s.progress[key], instance.ProgressAt = *progress, &at
		s.ProgressWrites++
	}
	return nil
}

func (s *Store) EndInstance(_ context.Context, platformExpID, agentID, name string, how db.InstanceEnd, exitLog string, limit time.Duration, maxRestarts int) (domain.AgentInstance, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.WriteErr[agentID]; err != nil {
		return domain.AgentInstance{}, false, err
	}
	instance := s.Find(platformExpID, agentID)
	if instance.Stopped || name == "" || instance.Name != name {
		return domain.AgentInstance{}, false, nil
	}
	now := epoch.Add(s.clock)
	var due bool
	switch how {
	case db.InstanceExited:
		due = true
	case db.InstanceGone:
		due = instance.SeenAt != nil
	case db.InstanceNeverSeen:
		due = instance.SeenAt == nil && s.clock-s.askedAt[platformExpID+"/"+agentID] >= limit
	case db.InstanceStillStarting:
		due = instance.SeenAt != nil && now.Sub(*instance.SeenAt) >= limit
	case db.InstanceSilent:
		since := instance.SeenAt
		if instance.ProgressAt != nil {
			since = instance.ProgressAt
		}
		due = instance.Reported == domain.InstanceRunning && since != nil && now.Sub(*since) >= limit
	}
	if !due {
		return domain.AgentInstance{}, false, nil
	}
	instance.Name, instance.ExitedAt, instance.ExitLog, instance.EndReason, instance.Reported = "", &now, exitLog, string(how), ""
	if instance.Restarts >= maxRestarts {
		instance.Stopped = true
	} else {
		instance.Restarts++
	}
	return *instance, true, nil
}

func (s *Store) SetLauncherHolds(_ context.Context, launcher string, holds bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Holds == nil {
		s.Holds = map[string]bool{}
	}
	s.Holds[launcher] = holds
	return nil
}
