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

// Editable is the fields of pe that caller may change now, the one rule PATCH refuses by and a
// form locks its fields by.
func Editable(pe *domain.PlatformExperiment, caller auth.Principal) []string {
	fields := fieldsIn(pe.Status)
	switch caller.Kind {
	case auth.KindCoordinator:
		return slices.DeleteFunc(fields, func(f string) bool { return !slices.Contains(coordinatorFields, f) })
	case auth.KindUser, auth.KindKey, auth.KindStaff:
		return fields
	}
	return nil
}

// notEditable refuses the fields of pe that no PATCH changes in its status.
func notEditable(pe *domain.PlatformExperiment, fields []string) error {
	var message string
	switch pe.Status {
	case domain.PlatformExpDraft:
		message = "A draft's plan comes from its planner. Change the goal, or edit the proposal before you accept it."
	case domain.PlatformExpClosed:
		message = fmt.Sprintf("platform experiment %s is closed and nothing of it changes.", pe.ID)
	default:
		message = fmt.Sprintf("%s can't change while it is %s.", strings.Join(fields, ", "), pe.Status)
	}
	return refusal.Refuse(http.StatusConflict, "not_editable", message)
}
