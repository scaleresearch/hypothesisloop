package platformexperiments

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// fieldsIn is what a PATCH changes of a platform experiment in status. A draft's plan comes from
// its planner and is edited in the accept, and once it runs admission and the stage ladder have
// acted on the rest.
func fieldsIn(status domain.PlatformExperimentStatus) []string {
	switch status {
	case domain.PlatformExpOpen:
		return []string{"name", "description", "budget_accelerator_hours", "max_agents", "researchers", "max_concurrent_accelerators",
			"metrics", "report_interval_seconds", "starts_at", "duration_hours", "baseline_job_id", "baseline_reason"}
	case domain.PlatformExpRunning:
		return []string{"name", "description", "duration_hours", "max_agents", "researchers", "baseline_job_id", "baseline_reason"}
	}
	return nil
}

// coordinatorFields is all a coordinator's token changes: its machine is rented by the platform,
// so it changes the brief and the baseline and nothing a run's cost or length follows from.
var coordinatorFields = []string{"description", "baseline_job_id", "baseline_reason"}

// goalEditable says the goal of pe changes: it has one, which a person changes until it closes.
func goalEditable(pe *domain.PlatformExperiment) bool {
	return pe.Goal != "" && pe.Status != domain.PlatformExpClosed
}

// Editable is the fields of pe that caller may change now, the one rule PATCH refuses by and a
// form locks its fields by. The goal is a person's.
func Editable(pe *domain.PlatformExperiment, caller auth.Principal) []string {
	fields := fieldsIn(pe.Status)
	switch caller.Kind {
	case auth.KindCoordinator:
		return slices.DeleteFunc(fields, func(f string) bool { return !slices.Contains(coordinatorFields, f) })
	case auth.KindUser, auth.KindKey, auth.KindStaff:
		if goalEditable(pe) {
			fields = append([]string{"goal"}, fields...)
		}
		return fields
	}
	return nil
}

// Permit sets on pe what caller may change of it and do to it now: nothing when caller does not
// write it.
func Permit(pe *domain.PlatformExperiment, caller auth.Principal, writes bool) {
	pe.Editable, pe.Actions = []string{}, []domain.PlatformExperimentAction{}
	if writes {
		pe.Editable = append(pe.Editable, Editable(pe, caller)...)
		pe.Actions = append(pe.Actions, actions(pe, caller)...)
	}
}

// actions is what caller, who writes pe, may do to it now, by the rules the writes refuse by.
func actions(pe *domain.PlatformExperiment, caller auth.Principal) []domain.PlatformExperimentAction {
	var actions []domain.PlatformExperimentAction
	if auth.AudienceAgent.Admits(caller, "") && caller.Actor().Kind != "" && pe.Status != domain.PlatformExpClosed {
		actions = append(actions, domain.PlatformExpActionAddIdea)
	}
	if !auth.AudienceCoordinator.Admits(caller, "") {
		return actions
	}
	actions = append(actions, domain.PlatformExpActionRemove)
	switch {
	case closes(pe) != nil:
	case pe.Status == domain.PlatformExpDraft:
		actions = append(actions, domain.PlatformExpActionAbandon)
	default:
		actions = append(actions, domain.PlatformExpActionClose)
	}
	if acceptable(pe) == nil {
		actions = append(actions, domain.PlatformExpActionAccept)
	}
	if takesSignup(pe.Status, domain.SignupRoleCompetitor) {
		actions = append(actions, domain.PlatformExpActionAddAgent)
	}
	return actions
}

// SignupActions is what caller may do now to the agent of signup in pe: nothing when caller does
// not write pe.
func SignupActions(pe *domain.PlatformExperiment, signup domain.AgentSignup, caller auth.Principal, writes bool) []domain.SignupAction {
	actions := []domain.SignupAction{}
	if !writes || !auth.AudienceCoordinator.Admits(caller, "") {
		return actions
	}
	if takesMessages(signup) {
		actions = append(actions, domain.SignupActionMessage)
	}
	if removesAgent(pe, signup, caller.Actor()) == nil {
		actions = append(actions, domain.SignupActionRemove)
	}
	if !signup.Hosted {
		actions = append(actions, domain.SignupActionShowToken)
	}
	return actions
}

// takesMessages says the agent of signup reads a message: it is no planner and its run goes on.
// The store refuses a message by the same rule (db.Store.SendAgentMessages).
func takesMessages(signup domain.AgentSignup) bool {
	return signup.Role != domain.SignupRolePlanner && signup.Ended == ""
}

// removesAgent refuses by taking the agent of signup out of pe: once pe is no longer open, and to a
// coordinator's token one it did not add itself.
func removesAgent(pe *domain.PlatformExperiment, signup domain.AgentSignup, by domain.Actor) error {
	if pe.Status != domain.PlatformExpOpen {
		return refuseSignup(409, "experiment_started", "experiment is %s: once started, an agent leaves by being cut or by the experiment closing", pe.Status)
	}
	if by.Kind == domain.ActorCoordinator && (signup.Actor == nil || signup.Actor.Kind != domain.ActorCoordinator || signup.Actor.ID != by.ID) {
		return refuseSignup(403, "agent_not_added_by_coordinator", "agent %s was not added by this platform experiment's coordinator, and a coordinator's token removes only the agents it added itself: its owner removes this one", signup.AgentID)
	}
	return nil
}

// closes refuses to close pe once it is closed.
func closes(pe *domain.PlatformExperiment) error {
	if pe.Status == domain.PlatformExpClosed {
		return invalidTransition("platform experiment %s is already closed", pe.ID)
	}
	return nil
}

// acceptable refuses to accept pe unless it is a draft whose planner's proposal stands.
func acceptable(pe *domain.PlatformExperiment) error {
	switch {
	case pe.Status != domain.PlatformExpDraft:
		return invalidTransition("platform experiment %s is %s: only a draft is accepted", pe.ID, pe.Status)
	case pe.DeclinedAt != nil:
		return invalidTransition("draft %s has no proposal to accept: its planner declined (declined_reason). Change its goal with PATCH /platform-experiments/%s, or close it", pe.ID, pe.ID)
	case pe.ProposedAt == nil:
		return invalidTransition("draft %s has no proposal to accept yet: its planner is still working", pe.ID)
	}
	return nil
}

// takesSignup says an agent of role joins a platform experiment in status: any while it is open,
// and once it runs a researcher only.
func takesSignup(status domain.PlatformExperimentStatus, role domain.SignupRole) bool {
	return status == domain.PlatformExpOpen || status == domain.PlatformExpRunning && role == domain.SignupRoleCompetitor
}

// notEditable refuses the fields of pe that no PATCH changes in its status.
func notEditable(pe *domain.PlatformExperiment, fields []string) error {
	var message string
	switch pe.Status {
	case domain.PlatformExpDraft:
		message = "A draft's plan comes from its planner. Change the goal, or edit the proposal before you accept it."
	case domain.PlatformExpClosed:
		message = fmt.Sprintf("platform experiment %s is closed and nothing of it changes.", pe.ID)
	case domain.PlatformExpOpen, domain.PlatformExpRunning:
		if pe.Goal == "" && slices.Contains(fields, "goal") {
			message = fmt.Sprintf("platform experiment %s was created whole and has no goal to change.", pe.ID)
		} else {
			message = fmt.Sprintf("%s can't change while it is %s.", strings.Join(fields, ", "), pe.Status)
		}
	}
	return refusal.Refuse(http.StatusConflict, "not_editable", message)
}
