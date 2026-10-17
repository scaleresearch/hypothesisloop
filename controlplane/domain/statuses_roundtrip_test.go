package domain

import "testing"

// A filter on a value the platform does not hold is refused before it reaches a store, so each
// check takes every listed value and nothing else, case included.
func TestTheValueChecksTakeTheListsAndNothingElse(t *testing.T) {
	for _, s := range ExperimentStatuses {
		if !ValidExperimentStatus(s) {
			t.Errorf("%s refused", s)
		}
	}
	for _, s := range PlatformExperimentStatuses {
		if !ValidPlatformExperimentStatus(s) {
			t.Errorf("%s refused", s)
		}
	}
	for _, s := range HypothesisStatuses {
		if !ValidHypothesisStatus(s) {
			t.Errorf("%s refused", s)
		}
	}
	for _, r := range OrgRoles {
		if !ValidOrgRole(r) {
			t.Errorf("%s refused", r)
		}
	}
	for _, bad := range []string{"", "running", "Completed", "CANCELED"} {
		if ValidExperimentStatus(ExperimentStatus(bad)) {
			t.Errorf("job status %q taken", bad)
		}
	}
	for _, bad := range []string{"", "Open", "RUNNING", "archived"} {
		if ValidPlatformExperimentStatus(PlatformExperimentStatus(bad)) {
			t.Errorf("platform experiment status %q taken", bad)
		}
	}
	for _, bad := range []string{"", "Open", "done"} {
		if ValidHypothesisStatus(HypothesisStatus(bad)) {
			t.Errorf("hypothesis status %q taken", bad)
		}
	}
	for _, bad := range []string{"", "Owner", "admin"} {
		if ValidOrgRole(OrgRole(bad)) {
			t.Errorf("org role %q taken", bad)
		}
	}
}

// A role a person sends reads back as itself, empty is a competitor, and the planner, which only
// the platform signs up, and anything unknown are refused.
func TestARequestedRoleReadsBackAsItself(t *testing.T) {
	for _, r := range []SignupRole{SignupRoleCompetitor, SignupRoleBaseline, SignupRoleReviewer} {
		if got, err := ParseSignupRole(string(r)); err != nil || got != r {
			t.Errorf("ParseSignupRole(%q) = %q, %v", r, got, err)
		}
	}
	if got, err := ParseSignupRole(""); err != nil || got != SignupRoleCompetitor {
		t.Errorf("ParseSignupRole(\"\") = %q, %v, want competitor", got, err)
	}
	for _, bad := range []string{string(SignupRolePlanner), "judge", "Competitor"} {
		if _, err := ParseSignupRole(bad); err == nil {
			t.Errorf("ParseSignupRole(%q) taken", bad)
		}
	}
}
