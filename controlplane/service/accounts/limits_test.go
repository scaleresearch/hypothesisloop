package accounts

import (
	"fmt"
	"net/http"
	"testing"
)

// The home a person starts with is one of the orgs they may own, and the default project one of
// the projects an org may hold.
func TestAnAccountCreatesUpToItsLimitsAndIsToldWhy(t *testing.T) {
	w := newWorld(t)
	for i := 2; i <= testLimits.MaxOrgsPerUser; i++ {
		w.ok("owner", "POST", "/orgs", name{"name": fmt.Sprintf("org %d", i)}, http.StatusCreated, nil)
	}
	w.refusedWith("owner", "POST", "/orgs", name{"name": "one more"}, http.StatusConflict, ReasonMaxOrgsReached)

	for held := 2; held < testLimits.MaxProjectsPerOrg; held++ {
		w.ok("owner", "POST", "/orgs/"+w.orgA+"/projects", name{"name": fmt.Sprintf("project %d", held)}, http.StatusCreated, nil)
	}
	before := snapshot(t, w.store)
	w.refusedWith("owner", "POST", "/orgs/"+w.orgA+"/projects", name{"name": "one more"}, http.StatusConflict, ReasonMaxProjectsReached)
	w.refusedWith("org-key", "POST", "/orgs/"+w.orgA+"/projects", name{"name": "one more"}, http.StatusConflict, ReasonMaxProjectsReached)
	if snapshot(t, w.store) != before {
		t.Fatal("a create refused at the limit changed the store")
	}
}

// Staff are held to neither limit, whoever they create for.
func TestStaffCreatePastTheLimits(t *testing.T) {
	w := newWorld(t)
	w.fillOrgs()
	w.fillProjects(w.orgA)
	w.ok("staff", "POST", "/orgs", map[string]any{"name": "a fourth for the owner", "owner_user_id": "u-owner"}, http.StatusCreated, nil)
	w.ok("staff-user", "POST", "/orgs", name{"name": "a fourth of their own"}, http.StatusCreated, nil)
	w.ok("staff", "POST", "/orgs/"+w.orgA+"/projects", name{"name": "past the limit"}, http.StatusCreated, nil)
	w.refusedWith("owner", "POST", "/orgs", name{"name": "still refused"}, http.StatusConflict, ReasonMaxOrgsReached)
}

// The limits are told once, on /me, and what is used of them is counted from the orgs and
// projects of the same answer.
func TestMeStatesTheLimitsOfACallerHeldToThem(t *testing.T) {
	w := newWorld(t)
	for _, caller := range []string{"owner", "member", "org-key", "project-key"} {
		var me Me
		w.ok(caller, "GET", "/me", nil, http.StatusOK, &me)
		if me.Limits == nil || *me.Limits != testLimits {
			t.Errorf("%s: limits = %+v, want = %+v", caller, me.Limits, testLimits)
		}
	}
	for _, caller := range []string{"staff", "staff-user"} {
		var me Me
		w.ok(caller, "GET", "/me", nil, http.StatusOK, &me)
		if me.Limits != nil {
			t.Errorf("%s: limits = %+v, want none: staff are held to no limit", caller, me.Limits)
		}
	}
	var me Me
	w.ok("owner", "GET", "/me", nil, http.StatusOK, &me)
	owned, inOrgA := 0, 0
	for _, org := range me.Orgs {
		if org.Role == "owner" {
			owned++
		}
	}
	for _, project := range me.Projects {
		if project.OrgID == w.orgA {
			inOrgA++
		}
	}
	if owned != 1 || inOrgA != 2 {
		t.Fatalf("what the owner uses, counted from /me: %d orgs owned and %d projects in org a, want 1 and 2", owned, inOrgA)
	}
}
