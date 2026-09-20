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

	clock   time.Duration
	askedAt map[string]time.Duration
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
	instance.Name, instance.Launch, instance.SeenAt = name, launch, nil
	if s.askedAt == nil {
		s.askedAt = map[string]time.Duration{}
	}
	s.askedAt[platformExpID+"/"+agentID] = s.clock
	return nil
}

func (s *Store) SeeInstance(_ context.Context, platformExpID, agentID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.WriteErr[agentID]; err != nil {
		return err
	}
	if instance := s.Find(platformExpID, agentID); name != "" && instance.Name == name && instance.SeenAt == nil {
		at := epoch.Add(s.clock)
		instance.SeenAt = &at
	}
	return nil
}

func (s *Store) EndInstance(_ context.Context, platformExpID, agentID, name string, how db.InstanceEnd, exitLog string, bootLimit time.Duration, maxRestarts int) (domain.AgentInstance, bool, error) {
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
		due = instance.SeenAt == nil && s.clock-s.askedAt[platformExpID+"/"+agentID] >= bootLimit
	case db.InstanceStillStarting:
		due = instance.SeenAt != nil && now.Sub(*instance.SeenAt) >= bootLimit
	}
	if !due {
		return domain.AgentInstance{}, false, nil
	}
	instance.Name, instance.ExitedAt, instance.ExitLog = "", &now, exitLog
	if instance.Restarts >= maxRestarts {
		instance.Stopped = true
	} else {
		instance.Restarts++
	}
	return *instance, true, nil
}
