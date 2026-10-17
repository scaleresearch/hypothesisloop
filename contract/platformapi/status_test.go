package platformapi

import (
	"slices"
	"testing"
)

func distinct[S comparable](t *testing.T, name string, values []S) {
	t.Helper()
	seen := map[S]bool{}
	for _, v := range values {
		if seen[v] {
			t.Errorf("%s holds %v twice", name, v)
		}
		seen[v] = true
	}
}

// A job has ended exactly when its status is one of EndedStatuses, each of which is a status a job
// is answered with, and no list names a value twice.
func TestAJobEndsInExactlyTheEndedStatuses(t *testing.T) {
	distinct(t, "ExperimentStatuses", ExperimentStatuses)
	distinct(t, "EndedStatuses", EndedStatuses)
	distinct(t, "PlatformExperimentStatuses", PlatformExperimentStatuses)
	distinct(t, "HypothesisStatuses", HypothesisStatuses)
	distinct(t, "OrgRoles", OrgRoles)
	distinct(t, "SignupRoles", SignupRoles)
	for _, s := range EndedStatuses {
		if !slices.Contains(ExperimentStatuses, s) {
			t.Errorf("ended status %s is no status a job is answered with", s)
		}
	}
	ended := 0
	for _, s := range ExperimentStatuses {
		if s.Ended() {
			ended++
		}
		if s.Ended() != slices.Contains(EndedStatuses, s) {
			t.Errorf("%s: Ended() = %v", s, s.Ended())
		}
	}
	if ended == len(ExperimentStatuses) || ended == 0 {
		t.Errorf("%d of %d statuses ended, want some of them", ended, len(ExperimentStatuses))
	}
	for _, s := range []ExperimentStatus{"", "completed", "DONE", "CANCELED"} {
		if s.Ended() {
			t.Errorf("unknown status %q reads as ended", s)
		}
	}
}
