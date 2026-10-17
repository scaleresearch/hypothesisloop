package accounts

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestAnOrgListsWhatItsAdministratorsAndMembersMayDoToIt(t *testing.T) {
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

var (
	owner     = auth.Principal{Kind: auth.KindUser, UserID: "u1"}
	member    = auth.Principal{Kind: auth.KindUser, UserID: "u2"}
	orgKey    = auth.Principal{Kind: auth.KindKey, KeyID: "k1"}
	projKey   = auth.Principal{Kind: auth.KindKey, KeyID: "k2", ProjectID: "prj_1"}
	staffUser = auth.Principal{Kind: auth.KindStaff, UserID: "u9"}
)

func roleOf(p auth.Principal) domain.OrgRole {
	switch p.UserID {
	case "u1":
		return domain.OrgRoleOwner
	case "u2":
		return domain.OrgRoleMember
	}
	return ""
}

func TestAMemberListsWhatTheCallerMayDoToThemAndTheLastOwnerNothing(t *testing.T) {
	bob := &domain.OrgMember{UserID: "u2", Role: domain.OrgRoleMember}
	ada := &domain.OrgMember{UserID: "u1", Role: domain.OrgRoleOwner}
	for _, c := range []struct {
		name   string
		caller auth.Principal
		of     *domain.OrgMember
		owners int
		want   string
	}{
		{"owner on a member", owner, bob, 1, "[set_role remove]"},
		{"member on themselves", member, bob, 1, "[remove]"},
		{"member on the owner", member, ada, 2, "[]"},
		{"owner on themselves, the last owner", owner, ada, 1, "[]"},
		{"owner on themselves, one of two", owner, ada, 2, "[set_role remove]"},
		{"key of a project", projKey, bob, 1, "[]"},
		{"key of the org", orgKey, bob, 1, "[set_role remove]"},
	} {
		if got := fmt.Sprint(memberActions(c.caller, roleOf(c.caller), c.of, c.owners)); got != c.want {
			t.Errorf("%s: got = %s, want = %s", c.name, got, c.want)
		}
	}
}

func TestAKeyListsRevokeForWhoeverEndsItUntilItIsRevoked(t *testing.T) {
	at := time.Now()
	keys := []*domain.APIKey{
		{ID: "k1", Actor: domain.Actor{ID: "u1"}},
		{ID: "k2", Actor: domain.Actor{ID: "u1"}, MintedBy: "k1"},
		{ID: "k3", Actor: domain.Actor{ID: "u2"}},
		{ID: "k4", Actor: domain.Actor{ID: "u2"}, RevokedAt: &at},
	}
	for _, c := range []struct {
		name   string
		caller auth.Principal
		want   string
	}{
		{"owner", owner, "k1 k2 k3"},
		{"member, who made k3", member, "k3"},
		{"key, which ends itself and what it minted", orgKey, "k1 k2"},
		{"staff", staffUser, "k1 k2 k3"},
	} {
		var revokes []string
		for _, k := range keys {
			if len(keyActions(c.caller, roleOf(c.caller), k, keys)) > 0 {
				revokes = append(revokes, k.ID)
			}
		}
		if got := strings.Join(revokes, " "); got != c.want {
			t.Errorf("%s: got = %s, want = %s", c.name, got, c.want)
		}
	}
}

func TestAClusterListsWhatTheCallerMayDoToItForTheOrg(t *testing.T) {
	own := &domain.Cluster{Name: "gpu", OrgID: "org_a"}
	shared := &domain.Cluster{Name: "pool", OrgID: "org_p", Shared: true}
	for _, c := range []struct {
		name    string
		caller  auth.Principal
		cluster *domain.Cluster
		want    string
	}{
		{"owner, its own", owner, own, "[enable edit remove]"},
		{"owner, a shared one", owner, shared, "[enable]"},
		{"member", member, own, "[]"},
		{"key of a project", projKey, own, "[]"},
		{"staff, its own", staffUser, own, "[enable edit remove share]"},
		{"staff, a shared one of another org", staffUser, shared, "[enable]"},
	} {
		if got := fmt.Sprint(clusterActions(c.cluster, "org_a", c.caller, roleOf(c.caller))); got != c.want {
			t.Errorf("%s: got = %s, want = %s", c.name, got, c.want)
		}
	}
}

func TestAnAgentTypeListsWhatTheCallerMayDoToIt(t *testing.T) {
	at := time.Now()
	ownType := domain.CurrentAgentType{AgentType: domain.AgentType{ID: "t1", OrgID: "org_a", Role: domain.AgentRoleResearcher}}
	archived := ownType
	archived.ArchivedAt = &at
	builtin := domain.CurrentAgentType{AgentType: domain.AgentType{ID: "b1", Role: domain.AgentRoleResearcher, Builtin: true}}
	coordinator := domain.CurrentAgentType{AgentType: domain.AgentType{ID: "b2", Role: domain.AgentRoleCoordinator, Builtin: true}}
	for _, c := range []struct {
		name   string
		caller auth.Principal
		t      domain.CurrentAgentType
		want   string
	}{
		{"owner, its own", owner, ownType, "[edit archive restore duplicate]"},
		{"owner, archived", owner, archived, "[duplicate]"},
		{"owner, built-in", owner, builtin, "[duplicate]"},
		{"owner, the built-in coordinator", owner, coordinator, "[]"},
		{"member", member, ownType, "[]"},
		{"key of a project", projKey, ownType, "[]"},
	} {
		if got := fmt.Sprint(agentTypeActions(c.caller, roleOf(c.caller), c.t)); got != c.want {
			t.Errorf("%s: got = %s, want = %s", c.name, got, c.want)
		}
	}
}
