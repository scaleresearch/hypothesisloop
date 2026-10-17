package domain

import (
	"fmt"
	"maps"
	"slices"
)

// Waiter is whose move a platform experiment that is not closed waits for.
type Waiter string

const (
	// WaitsForOwner is a person of the experiment's organisation: nothing an agent or the
	// platform does changes the state.
	WaitsForOwner Waiter = "owner"
	// WaitsForCoordinator is the experiment's coordinator.
	WaitsForCoordinator Waiter = "coordinator"
	// WaitsForPlatform is nobody's move: the platform, an agent already at work or the clock
	// brings the next state.
	WaitsForPlatform Waiter = "platform"
)

// Waiters is everyone a platform experiment can wait for.
var Waiters = []Waiter{WaitsForOwner, WaitsForCoordinator, WaitsForPlatform}

// statusWaiters is, for every reason a platform experiment that is not closed answers as its
// status_reason, and for the reason an agent of its roster adds, who acts on it. It is the one
// place that says so.
var statusWaiters = map[string]Waiter{
	"repository_creating": WaitsForPlatform,
	"planning":            WaitsForPlatform,
	"revising":            WaitsForPlatform,
	"accepting":           WaitsForPlatform,
	"proposed":            WaitsForOwner,
	"declined":            WaitsForOwner,

	"baseline_measuring":  WaitsForPlatform,
	"baseline_declaring":  WaitsForPlatform,
	"baseline_required":   WaitsForCoordinator,
	"baseline_undeclared": WaitsForCoordinator,
	// What the platform's own declaration of the measured job is refused for. Another baseline
	// agent repairs a job that measured too little, and only the owner changes a bound or pays.
	"no_baseline":                  WaitsForCoordinator,
	"baseline_not_completed":       WaitsForCoordinator,
	"baseline_not_on_branch":       WaitsForCoordinator,
	"baseline_without_finding":     WaitsForCoordinator,
	"baseline_missing_metric":      WaitsForCoordinator,
	"baseline_environment_differs": WaitsForCoordinator,
	"baseline_not_in_environment":  WaitsForCoordinator,
	// The baseline agent declares the job of its strongest configuration: the coordinator
	// declares one, or adds a baseline agent to measure it.
	"baseline_weaker_than_own_job": WaitsForCoordinator,
	"baseline_breaks_constraint":   WaitsForOwner,
	"budget_below_baseline":        WaitsForOwner,

	// It starts by itself when starts_at arrives. Its owner may move starts_at and need not.
	"starts_at_ahead": WaitsForPlatform,
	"no_researchers":  WaitsForOwner,
	"budget_spent":    WaitsForOwner,
	"starting":        WaitsForPlatform,

	"agents_starting": WaitsForPlatform,
	"running":         WaitsForPlatform,
	// Nobody is left to submit a job and the run only waits for its ends_at: whoever coordinates
	// writes the summary and closes it. The platform closes nothing before ends_at by itself.
	"researchers_done": WaitsForCoordinator,

	"instance_starting": WaitsForPlatform,
}

// WaitsFor is who acts on a platform experiment that answers reason. Where it has no coordinator
// the platform hosts, a person coordinates it, so what its coordinator would do is its owner's.
func WaitsFor(reason string, coordinatorInAgentSandbox bool) (Waiter, error) {
	who, known := statusWaiters[reason]
	if !known {
		return "", fmt.Errorf("status reason %q names nobody it waits for", reason)
	}
	if who == WaitsForCoordinator && !coordinatorInAgentSandbox {
		return WaitsForOwner, nil
	}
	return who, nil
}

// StatusReasons is every reason a platform experiment that is not closed answers as its
// status_reason, sorted.
func StatusReasons() []string { return slices.Sorted(maps.Keys(statusWaiters)) }
