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
	// CoordinatorWindDown is the last one the coordinators were listed with.
	Listed              []string
	WindDown            time.Duration
	CoordinatorWindDown time.Duration
	// Over holds, by platform experiment and agent id joined with a slash, the agents whose run
	// ended longer ago than the wind-down, and why: the launcher is no longer asked to keep
	// them, and the next question releases their machines.
	Over map[string]domain.RunEnd

	// ProgressWrites counts the times a progress marker was written down.
	ProgressWrites int
	// Holds is what each launcher's last question left recorded of it holding machines.
	Holds map[string]bool
	// Activity holds, by platform experiment and agent id joined with a slash, what the machine
	// asked for last said its agent is doing, and ActivityWrites counts the times one was written.
	Activity       map[string]domain.AgentActivity
	ActivityWrites int

	clock       time.Duration
	askedAt     map[string]time.Duration
	progress    map[string]string
	restartedAt map[string][]time.Duration
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

func (s *Store) ListLauncherCoordinators(_ context.Context, launcher string, windDown time.Duration) ([]db.LauncherCoordinator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CoordinatorWindDown = windDown
	listed := []db.LauncherCoordinator{}
	for _, coordinator := range s.Coordinators {
		if coordinator.Launcher == launcher {
			listed = append(listed, coordinator)
		}
	}
	return listed, s.Err
}

// ReleaseInstances records the machines of the agents in Over as released by the platform.
func (s *Store) ReleaseInstances(_ context.Context, launcher string, _, _ time.Duration) error {
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
		delete(s.Activity, agent.PlatformExperimentID+"/"+agent.AgentID)
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
		instance.Stopped = false
		delete(s.restartedAt, platformExpID+"/"+agentID)
	}
	instance.Name, instance.Launch, instance.SeenAt, instance.Reported, instance.ProgressAt = name, launch, nil, "", nil
	delete(s.progress, platformExpID+"/"+agentID)
	delete(s.Activity, platformExpID+"/"+agentID)
	if s.askedAt == nil {
		s.askedAt = map[string]time.Duration{}
	}
	s.askedAt[platformExpID+"/"+agentID] = s.clock
	return nil
}

func (s *Store) ObserveInstance(_ context.Context, platformExpID, agentID, name, state string, progress *string, progressLimit time.Duration, activity string) error {
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
	if activity != "" && activity != s.Activity[key].Text {
		if s.Activity == nil {
			s.Activity = map[string]domain.AgentActivity{}
		}
		s.Activity[key] = domain.AgentActivity{Text: activity, At: at}
		s.ActivityWrites++
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
	case db.InstanceExited, db.InstanceExpired:
		due = true
	case db.InstanceGone:
		due = instance.SeenAt != nil
	case db.InstanceNeverSeen:
		due = instance.SeenAt == nil && s.clock-s.askedAt[platformExpID+"/"+agentID] >= limit
	case db.InstanceStillStarting:
		due = instance.SeenAt != nil && now.Sub(*instance.SeenAt) >= limit
	case db.InstanceSilent, db.InstanceProgressUnreadable:
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
	delete(s.Activity, platformExpID+"/"+agentID)
	key := platformExpID + "/" + agentID
	var recent []time.Duration
	for _, at := range s.restartedAt[key] {
		if s.clock-at < domain.AgentInstanceRestartWindow {
			recent = append(recent, at)
		}
	}
	switch {
	case !how.SpendsRestart():
		instance.Renewals++
	case len(recent) >= maxRestarts:
		instance.Stopped = true
	default:
		instance.Restarts++
		recent = append(recent, s.clock)
	}
	if s.restartedAt == nil {
		s.restartedAt = map[string][]time.Duration{}
	}
	s.restartedAt[key] = recent
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
