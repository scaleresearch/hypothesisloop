package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
)

type roleBody struct {
	Role domain.OrgRole `json:"role" doc:"owner or member"`
}

func (a accountsAPI) registerMembers(doc *Doc) {
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "list-org-members", Method: "GET", Path: "/orgs/{id}/members",
		Summary: "List an org's members", Tags: []string{"accounts"},
		Description: "Oldest first, each with the role they hold. An owner changes everything in the org. A member reads everything in it, " +
			"creates and runs platform experiments and what goes on in them, and creates API keys bound to one project. " +
			"A member is named by user_id alone: names and emails are the identity provider's.",
	}, a.listMembers)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "set-org-member-role", Method: "PATCH", Path: "/orgs/{id}/members/{user_id}",
		Summary: "Change a member's role", Tags: []string{"accounts"},
		Description: "For an owner. An org keeps at least one owner: demoting the last one answers 409 with reason last_owner. " +
			"A person who stops being an owner loses what only an owner holds: their API keys of the whole org stop working and the invites they made are revoked.",
	}, a.setMemberRole)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "remove-org-member", Method: "DELETE", Path: "/orgs/{id}/members/{user_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Remove a member, or leave an org", Tags: []string{"accounts"},
		Description: "For an owner, and for any member naming themselves. Removing the last owner answers 409 with reason last_owner. " +
			"The API keys the person created in the org stop working at once and the invites they made are revoked. What they wrote stays.",
	}, a.removeMember)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "create-org-invite", Method: "POST", Path: "/orgs/{id}/invites", DefaultStatus: http.StatusCreated,
		Summary: "Invite a person into an org", Tags: []string{"accounts"},
		Description: "For an owner. Returns the invite's token once: the platform keeps its hash only and sends no email, so hand the token to the person yourself. " +
			"Whoever signs in and accepts it joins with the invite's role. It works once, for seven days, and until it is revoked.",
	}, a.createInvite)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "list-org-invites", Method: "GET", Path: "/orgs/{id}/invites",
		Summary: "List an org's pending invites", Tags: []string{"accounts"},
		Description: "For an owner. The invites that can still be accepted, newest first. No token is ever listed.",
	}, a.listInvites)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "revoke-org-invite", Method: "DELETE", Path: "/invites/{id}", DefaultStatus: http.StatusNoContent,
		Summary: "Revoke an invite", Tags: []string{"accounts"},
		Description: "For an owner of the invite's org. The invite can no longer be accepted. Revoking a revoked invite changes nothing. " +
			"One already accepted answers 409 with reason invite_used: remove the member instead.",
	}, a.revokeInvite)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "accept-org-invite", Method: "POST", Path: "/invites/accept", DefaultStatus: http.StatusCreated,
		Summary: "Accept an invite to an org", Tags: []string{"accounts"},
		Description: "For a signed-in person, who joins the invite's org with the invite's role. An API key is no person and is refused. " +
			"A token the platform does not know answers 404. One that cannot be accepted answers 409 with the reason: invite_used, invite_revoked, " +
			"invite_expired, or already_member when the person is in the org already, which leaves the invite unused.",
	}, a.acceptInvite)
}

func (a accountsAPI) listMembers(ctx context.Context, in *struct {
	ID string `path:"id"`
	accounts.Page
}) (*struct {
	TotalCount int `header:"X-Total-Count" doc:"The members of the org, ignoring limit and offset."`
	Body       []*domain.OrgMember
}, error) {
	members, total, err := a.accounts.Members(ctx, in.ID, in.Page)
	if err != nil {
		return nil, err
	}
	return &struct {
		TotalCount int `header:"X-Total-Count" doc:"The members of the org, ignoring limit and offset."`
		Body       []*domain.OrgMember
	}{TotalCount: total, Body: members}, nil
}

func (a accountsAPI) setMemberRole(ctx context.Context, in *struct {
	ID     string `path:"id"`
	UserID string `path:"user_id"`
	Body   roleBody
}) (*struct{ Body domain.OrgMember }, error) {
	member, err := a.accounts.SetMemberRole(ctx, in.ID, in.UserID, in.Body.Role)
	if err != nil {
		return nil, err
	}
	return &struct{ Body domain.OrgMember }{Body: *member}, nil
}

func (a accountsAPI) removeMember(ctx context.Context, in *struct {
	ID     string `path:"id"`
	UserID string `path:"user_id"`
}) (*struct{}, error) {
	if err := a.accounts.RemoveMember(ctx, in.ID, in.UserID); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func (a accountsAPI) createInvite(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body roleBody
}) (*struct{ Body accounts.CreatedInvite }, error) {
	invite, err := a.accounts.CreateInvite(ctx, in.ID, in.Body.Role)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.CreatedInvite }{Body: *invite}, nil
}

func (a accountsAPI) listInvites(ctx context.Context, in *struct {
	ID string `path:"id"`
	accounts.Page
}) (*struct {
	TotalCount int `header:"X-Total-Count" doc:"The pending invites of the org, ignoring limit and offset."`
	Body       []*domain.OrgInvite
}, error) {
	invites, total, err := a.accounts.Invites(ctx, in.ID, in.Page)
	if err != nil {
		return nil, err
	}
	return &struct {
		TotalCount int `header:"X-Total-Count" doc:"The pending invites of the org, ignoring limit and offset."`
		Body       []*domain.OrgInvite
	}{TotalCount: total, Body: invites}, nil
}

func (a accountsAPI) revokeInvite(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{}, error) {
	if err := a.accounts.RevokeInvite(ctx, in.ID); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}

func (a accountsAPI) acceptInvite(ctx context.Context, in *struct {
	Body struct {
		Token string `json:"token"`
	}
}) (*struct{ Body domain.OrgMember }, error) {
	member, err := a.accounts.AcceptInvite(ctx, in.Body.Token)
	if err != nil {
		return nil, err
	}
	return &struct{ Body domain.OrgMember }{Body: *member}, nil
}
