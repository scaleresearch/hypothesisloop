package scheduler

import (
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// JobActions is what caller may do to exp now: nothing when caller does not write its platform
// experiment.
func JobActions(exp *domain.Experiment, caller auth.Principal, writes bool) []domain.ExperimentAction {
	actions := []domain.ExperimentAction{}
	if writes && auth.AudienceAgent.Admits(caller, "") && (caller.Kind != auth.KindAgent || caller.AgentID == exp.AgentID) && cancels(exp.Status) {
		actions = append(actions, domain.ExperimentActionCancel)
	}
	return actions
}

// cancels says a job in status is cancelled: it has not ended.
func cancels(status domain.ExperimentStatus) bool {
	return status == domain.StatusQueued || status == domain.StatusSubmitted || status == domain.StatusRunning
}
