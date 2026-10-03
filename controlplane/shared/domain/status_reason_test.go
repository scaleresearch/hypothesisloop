package domain

import "testing"

// A coordinator read budget_below_baseline, which only the owner clears, and waited out its
// baseline limit, and a person read baseline_required and waited for a coordinator that was
// already waiting for them. Each reason names one actor.
func TestEveryStatusReasonNamesWhoActs(t *testing.T) {
	for reason, want := range map[string]Waiter{
		"proposed": WaitsForOwner, "declined": WaitsForOwner, "planning": WaitsForPlatform,
		"budget_below_baseline": WaitsForOwner, "baseline_breaks_constraint": WaitsForOwner, "no_researchers": WaitsForOwner, "budget_spent": WaitsForOwner,
		"baseline_required": WaitsForCoordinator, "baseline_undeclared": WaitsForCoordinator, "baseline_missing_metric": WaitsForCoordinator, "researchers_done": WaitsForCoordinator,
		"baseline_measuring": WaitsForPlatform, "starts_at_ahead": WaitsForPlatform, "starting": WaitsForPlatform, "agents_starting": WaitsForPlatform,
	} {
		if got, err := WaitsFor(reason, true); err != nil || got != want {
			t.Errorf("%s with a hosted coordinator: got = %q, %v, want %s", reason, got, err, want)
		}
		// A person who coordinates is the coordinator.
		if want == WaitsForCoordinator {
			want = WaitsForOwner
		}
		if got, err := WaitsFor(reason, false); err != nil || got != want {
			t.Errorf("%s coordinated by a person: got = %q, %v, want %s", reason, got, err, want)
		}
	}
	if who, err := WaitsFor("a_reason_nobody_listed", true); err == nil {
		t.Errorf("a reason the table does not hold: got = %q, want an error", who)
	}
}
