package accounts

import (
	"fmt"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestAnOrgListsTheActionsItsAdministratorsAndMembersMayTake(t *testing.T) {
	for _, c := range []struct {
		name   string
		caller auth.Principal
		role   domain.OrgRole
		want   string
	}{
		{"owner", auth.Principal{Kind: auth.KindUser, UserID: "u1"}, domain.OrgRoleOwner, "[edit create_api_key]"},
		{"member", auth.Principal{Kind: auth.KindUser, UserID: "u1"}, domain.OrgRoleMember, "[create_api_key]"},
		{"key of the org", auth.Principal{Kind: auth.KindKey, KeyID: "k1"}, "", "[edit create_api_key]"},
		{"key of a project", auth.Principal{Kind: auth.KindKey, KeyID: "k1", ProjectID: "prj_1"}, "", "[create_api_key]"},
		{"staff signed in", auth.Principal{Kind: auth.KindStaff, UserID: "u9"}, "", "[edit create_api_key]"},
		{"staff token", auth.Principal{Kind: auth.KindStaff}, "", "[edit]"},
	} {
		if got := fmt.Sprint(orgActions(c.caller, &domain.Org{ID: "org_a", Role: c.role})); got != c.want {
			t.Errorf("%s: got = %s, want = %s", c.name, got, c.want)
		}
	}
}
