package accounts

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// refusedWith makes a request that must be refused with the status and the reason.
func (w *world) refusedWith(caller, method, path string, body any, status int, reason string) {
	w.t.Helper()
	var refusal struct{ Reason, Message string }
	w.ok(caller, method, path, body, status, &refusal)
	if refusal.Reason != reason || refusal.Message == "" {
		w.t.Fatalf("%s %s %s: got reason = %q message = %q, want reason = %q and a message", caller, method, path, refusal.Reason, refusal.Message, reason)
	}
}

func TestAnInviteBringsAPersonIntoTheOrgOnce(t *testing.T) {
	w := newWorld(t)
	w.store.users["u-new"] = true
	w.callers["newcomer"] = auth.Principal{Kind: auth.KindUser, UserID: "u-new", Name: "New"}

	var created CreatedInvite
	w.ok("owner", "POST", "/orgs/"+w.orgA+"/invites", name{"role": "member"}, http.StatusCreated, &created)
	if !strings.HasPrefix(created.ID, "inv-") || !strings.HasPrefix(created.Token, "hli_") || created.OrgID != w.orgA ||
		created.Role != domain.OrgRoleMember || created.Actor.ID != "u-owner" {
		t.Fatalf("created invite: got = %+v, want id inv-..., token hli_..., made by the owner for a member of %s", created, w.orgA)
	}
	if got := created.ExpiresAt.Sub(time.Now()); got < domain.InviteLifetime-time.Minute || got > domain.InviteLifetime {
		t.Fatalf("the invite expires in %v, want %v", got, domain.InviteLifetime)
	}
	if _, stored := w.store.inviteIDs[auth.HashAPIKey(created.Token)]; !stored || strings.Contains(snapshot(t, w.store), created.Token) {
		t.Fatal("the invite must be stored under the hash of its token, and the token nowhere")
	}

	rec := w.call("owner", "GET", "/orgs/"+w.orgA+"/invites", nil)
	var pending []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("list invites: got = %d %s, want a bare array", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Total-Count") != "2" || len(pending) != 2 || pending[0]["id"] != created.ID || pending[1]["id"] != w.inviteA {
		t.Fatalf("pending invites: got = %s with X-Total-Count %q, want the new one then the seeded one of 2", rec.Body, rec.Header().Get("X-Total-Count"))
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("the listing shows a token: %s", rec.Body)
	}

	var joined domain.OrgMember
	w.ok("newcomer", "POST", "/invites/accept", name{"token": created.Token}, http.StatusCreated, &joined)
	if joined.OrgID != w.orgA || joined.UserID != "u-new" || joined.Role != domain.OrgRoleMember {
		t.Fatalf("accepted: got = %+v, want u-new a member of %s", joined, w.orgA)
	}
	w.refusedWith("outsider", "POST", "/invites/accept", name{"token": created.Token}, http.StatusConflict, ReasonInviteUsed)
	w.refusedWith("owner", "DELETE", "/invites/"+created.ID, nil, http.StatusConflict, ReasonInviteUsed)

	rec = w.call("member", "GET", "/orgs/"+w.orgA+"/members?limit=2&offset=1", nil)
	var members []domain.OrgMember
	if err := json.Unmarshal(rec.Body.Bytes(), &members); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("list members: got = %d %s, want a bare array", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Total-Count") != "3" || len(members) != 2 || members[0].UserID != "u-member" || members[1].UserID != "u-new" {
		t.Fatalf("members after the first: got = %s with X-Total-Count %q, want u-member then u-new of 3", rec.Body, rec.Header().Get("X-Total-Count"))
	}
}

func TestAnInviteThatCannotBeAcceptedSaysWhy(t *testing.T) {
	w := newWorld(t)
	w.refusedWith("outsider", "POST", "/invites/accept", name{"token": "hli_never-issued"}, http.StatusNotFound, ReasonNotFound)
	w.refusedWith("owner", "POST", "/invites/accept", name{"token": w.inviteToken}, http.StatusConflict, ReasonAlreadyMember)
	if got := w.store.invites[w.inviteA]; got.AcceptedAt != nil {
		t.Fatal("an accept by someone already in the org used the invite up")
	}

	w.store.invites[w.inviteA].ExpiresAt = w.store.clock.Add(-time.Second)
	w.refusedWith("outsider", "POST", "/invites/accept", name{"token": w.inviteToken}, http.StatusConflict, ReasonInviteExpired)
	var pending []domain.OrgInvite
	w.ok("owner", "GET", "/orgs/"+w.orgA+"/invites", nil, http.StatusOK, &pending)
	if len(pending) != 0 {
		t.Fatalf("pending invites: got = %+v, want none: the only one expired", pending)
	}

	w.store.invites[w.inviteA].ExpiresAt = w.store.clock.Add(time.Hour)
	w.ok("owner", "DELETE", "/invites/"+w.inviteA, nil, http.StatusNoContent, nil)
	w.ok("owner", "DELETE", "/invites/"+w.inviteA, nil, http.StatusNoContent, nil)
	w.refusedWith("outsider", "POST", "/invites/accept", name{"token": w.inviteToken}, http.StatusConflict, ReasonInviteRevoked)
	if _, member := w.store.roles[w.orgA]["u-other"]; member {
		t.Fatal("a refused accept made a member")
	}
}

func TestABodyOrPageThatIsInvalidIsRefusedWithItsReason(t *testing.T) {
	w := newWorld(t)
	before := snapshot(t, w.store)
	w.refusedWith("owner", "POST", "/orgs/"+w.orgA+"/invites", name{"role": "admin"}, http.StatusUnprocessableEntity, ReasonInvalidRole)
	w.refusedWith("owner", "PATCH", "/orgs/"+w.orgA+"/members/u-member", name{"role": ""}, http.StatusUnprocessableEntity, ReasonInvalidRole)
	for _, query := range []string{"limit=201", "limit=-1", "offset=-1"} {
		w.refusedWith("owner", "GET", "/orgs/"+w.orgA+"/members?"+query, nil, http.StatusUnprocessableEntity, ReasonInvalidPage)
		w.refusedWith("owner", "GET", "/orgs/"+w.orgA+"/invites?"+query, nil, http.StatusUnprocessableEntity, ReasonInvalidPage)
	}
	w.refusedWith("owner", "PATCH", "/orgs/"+w.orgA+"/members/u-other", name{"role": "owner"}, http.StatusNotFound, ReasonNotFound)
	w.refusedWith("owner", "DELETE", "/orgs/"+w.orgA+"/members/u-other", nil, http.StatusNotFound, ReasonNotFound)
	if snapshot(t, w.store) != before {
		t.Fatal("a refused request changed the store")
	}
}

func TestAnOrgKeepsAnOwnerAndAMemberMayLeave(t *testing.T) {
	w := newWorld(t)
	members := "/orgs/" + w.orgA + "/members/"
	w.refusedWith("owner", "DELETE", members+"u-owner", nil, http.StatusConflict, ReasonLastOwner)
	w.refusedWith("owner", "PATCH", members+"u-owner", name{"role": "member"}, http.StatusConflict, ReasonLastOwner)

	if rec := w.call("member", "DELETE", members+"u-owner", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("a member removing the owner: got = %d %s, want = 403", rec.Code, rec.Body)
	}
	var promoted domain.OrgMember
	w.ok("owner", "PATCH", members+"u-member", name{"role": "owner"}, http.StatusOK, &promoted)
	if promoted.Role != domain.OrgRoleOwner || promoted.UserID != "u-member" {
		t.Fatalf("promoted: got = %+v, want u-member an owner", promoted)
	}
	// The invite the first owner made goes with their ownership.
	w.ok("member", "PATCH", members+"u-owner", name{"role": "member"}, http.StatusOK, nil)
	if w.store.invites[w.inviteA].RevokedAt == nil {
		t.Fatal("the invite made by an owner who was demoted is still pending")
	}
	w.ok("owner", "DELETE", members+"u-owner", nil, http.StatusNoContent, nil)
	if _, still := w.store.roles[w.orgA]["u-owner"]; still {
		t.Fatal("the one who left is still a member")
	}
	w.refusedWith("member", "DELETE", members+"u-member", nil, http.StatusConflict, ReasonLastOwner)
}

// A member holds keys of one project and ends only their own.
func TestAMemberMintsProjectKeysAndEndsTheirOwn(t *testing.T) {
	w := newWorld(t)
	var created CreatedAPIKey
	w.ok("member", "POST", "/orgs/"+w.orgA+"/api-keys", name{"name": "mine", "project_id": w.projectA2}, http.StatusCreated, &created)
	if created.Actor.ID != "u-member" || created.ProjectID != w.projectA2 {
		t.Fatalf("a member's key: got = %+v, want it made by u-member for %s", created.APIKey, w.projectA2)
	}
	if rec := w.call("member", "POST", "/orgs/"+w.orgA+"/api-keys", name{"name": "wide"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a member creating an org-wide key: got = %d %s, want = 403", rec.Code, rec.Body)
	}
	w.ok("member", "DELETE", "/api-keys/"+created.ID, nil, http.StatusOK, nil)
	for _, id := range []string{w.personKey, w.parentKey, "k-prj"} {
		if rec := w.call("member", "DELETE", "/api-keys/"+id, nil); rec.Code != http.StatusForbidden {
			t.Errorf("a member revoking %s, a key the owner made: got = %d %s, want = 403", id, rec.Code, rec.Body)
		}
	}
}
