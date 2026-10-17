package accounts

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
)

// The reasons a client tells a refused membership change or invite apart by.
const (
	ReasonNotFound      = "not_found"
	ReasonInvalidRole   = "invalid_role"
	ReasonInvalidPage   = "invalid_page"
	ReasonLastOwner     = "last_owner"
	ReasonAlreadyMember = "already_member"
	ReasonInviteUsed    = "invite_used"
	ReasonInviteRevoked = "invite_revoked"
	ReasonInviteExpired = "invite_expired"

	ReasonMaxOrgsReached     = "max_orgs_reached"
	ReasonMaxProjectsReached = "max_projects_reached"
)

func conflict(reason, message string) *api.Refusal {
	return api.Refuse(http.StatusConflict, reason, message)
}

func notFound(what string) *api.Refusal {
	return api.Refuse(http.StatusNotFound, ReasonNotFound, what+" not found")
}

func invalid(reason, message string) *api.Refusal {
	return api.Refuse(http.StatusUnprocessableEntity, reason, message)
}

const (
	defaultPageLimit = 20
	maxPageLimit     = 200
)

// Page is one page of a listing. Limit and Offset are the query parameters of every list here.
type Page struct {
	Limit  int `query:"limit" doc:"Page size, default 20, maximum 200. A larger one is refused."`
	Offset int `query:"offset" doc:"Rows to skip, for paging through the X-Total-Count total."`
}

func (p Page) bounds() (limit, offset int, err error) {
	if p.Limit < 0 || p.Limit > maxPageLimit || p.Offset < 0 {
		return 0, 0, invalid(ReasonInvalidPage, fmt.Sprintf("limit must be 1 to %d and offset must not be negative", maxPageLimit))
	}
	if p.Limit == 0 {
		return defaultPageLimit, p.Offset, nil
	}
	return p.Limit, p.Offset, nil
}

// CreatedInvite is an invite at the one moment its token exists outside the caller's hands.
type CreatedInvite struct {
	domain.OrgInvite
	Token string `json:"token" doc:"The secret the invited person accepts with. Returned by this call only: the platform stores its hash."`
}

type roleBody struct {
	Role domain.OrgRole `json:"role" doc:"owner or member"`
}

func validRole(role domain.OrgRole) error {
	if !domain.ValidOrgRole(role) {
		return invalid(ReasonInvalidRole, "role must be owner or member")
	}
	return nil
}

func registerMembers(doc *apidocs.Doc, h *Handler) {
	route(doc, h, huma.Operation{
		OperationID: "list-org-members", Method: "GET", Path: "/orgs/{id}/members",
		Summary: "List an org's members", Tags: []string{"accounts"},
		Description: "Oldest first, each with the role they hold. An owner changes everything in the org. A member reads everything in it, " +
			"creates and runs platform experiments and what goes on in them, and creates API keys bound to one project. " +
			"A member is named by user_id alone: names and emails are the identity provider's.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
		Page
	}) (*struct {
		TotalCount int `header:"X-Total-Count" doc:"The members of the org, ignoring limit and offset."`
		Body       []*domain.OrgMember
	}, error) {
		if _, err := h.policy.readOrg(ctx, in.ID); err != nil {
			return nil, err
		}
		limit, offset, err := in.bounds()
		if err != nil {
			return nil, err
		}
		members, total, err := h.accounts.ListOrgMembers(ctx, in.ID, limit, offset)
		if err != nil {
			return nil, err
		}
		return &struct {
			TotalCount int `header:"X-Total-Count" doc:"The members of the org, ignoring limit and offset."`
			Body       []*domain.OrgMember
		}{TotalCount: total, Body: members}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "set-org-member-role", Method: "PATCH", Path: "/orgs/{id}/members/{user_id}",
		Summary: "Change a member's role", Tags: []string{"accounts"},
		Description: "For an owner. An org keeps at least one owner: demoting the last one answers 409 with reason last_owner. " +
			"A person who stops being an owner loses what only an owner holds: their API keys of the whole org stop working and the invites they made are revoked.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		UserID string `path:"user_id"`
		Body   roleBody
	}) (*struct{ Body domain.OrgMember }, error) {
		if _, err := h.access.OrgAdmin(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := validRole(in.Body.Role); err != nil {
			return nil, err
		}
		member, err := h.accounts.SetOrgMemberRole(ctx, in.ID, in.UserID, in.Body.Role)
		if err != nil {
			return nil, err
		}
		return &struct{ Body domain.OrgMember }{Body: *member}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "remove-org-member", Method: "DELETE", Path: "/orgs/{id}/members/{user_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Remove a member, or leave an org", Tags: []string{"accounts"},
		Description: "For an owner, and for any member naming themselves. Removing the last owner answers 409 with reason last_owner. " +
			"The API keys the person created in the org stop working at once and the invites they made are revoked. What they wrote stays.",
	}, func(ctx context.Context, in *struct {
		ID     string `path:"id"`
		UserID string `path:"user_id"`
	}) (*struct{}, error) {
		if err := h.access.MemberRemoval(ctx, in.ID, in.UserID); err != nil {
			return nil, err
		}
		if err := h.accounts.RemoveOrgMember(ctx, in.ID, in.UserID); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "create-org-invite", Method: "POST", Path: "/orgs/{id}/invites", DefaultStatus: http.StatusCreated,
		Summary: "Invite a person into an org", Tags: []string{"accounts"},
		Description: "For an owner. Returns the invite's token once: the platform keeps its hash only and sends no email, so hand the token to the person yourself. " +
			"Whoever signs in and accepts it joins with the invite's role. It works once, for seven days, and until it is revoked.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body roleBody
	}) (*struct{ Body CreatedInvite }, error) {
		createdBy, err := h.access.Inviter(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		if err := validRole(in.Body.Role); err != nil {
			return nil, err
		}
		id, token, err := auth.NewInvite()
		if err != nil {
			return nil, err
		}
		invite := &domain.OrgInvite{ID: id, OrgID: in.ID, Role: in.Body.Role, Actor: domain.Actor{Kind: domain.ActorUser, ID: createdBy}, ExpiresAt: time.Now().UTC().Add(domain.InviteLifetime)}
		if err := h.accounts.CreateOrgInvite(ctx, invite, auth.HashAPIKey(token)); err != nil {
			return nil, err
		}
		return &struct{ Body CreatedInvite }{Body: CreatedInvite{OrgInvite: *invite, Token: token}}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "list-org-invites", Method: "GET", Path: "/orgs/{id}/invites",
		Summary: "List an org's pending invites", Tags: []string{"accounts"},
		Description: "For an owner. The invites that can still be accepted, newest first. No token is ever listed.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
		Page
	}) (*struct {
		TotalCount int `header:"X-Total-Count" doc:"The pending invites of the org, ignoring limit and offset."`
		Body       []*domain.OrgInvite
	}, error) {
		if _, err := h.access.OrgAdmin(ctx, in.ID); err != nil {
			return nil, err
		}
		limit, offset, err := in.bounds()
		if err != nil {
			return nil, err
		}
		invites, total, err := h.accounts.ListPendingOrgInvites(ctx, in.ID, limit, offset)
		if err != nil {
			return nil, err
		}
		return &struct {
			TotalCount int `header:"X-Total-Count" doc:"The pending invites of the org, ignoring limit and offset."`
			Body       []*domain.OrgInvite
		}{TotalCount: total, Body: invites}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "revoke-org-invite", Method: "DELETE", Path: "/invites/{id}", DefaultStatus: http.StatusNoContent,
		Summary: "Revoke an invite", Tags: []string{"accounts"},
		Description: "For an owner of the invite's org. The invite can no longer be accepted. Revoking a revoked invite changes nothing. " +
			"One already accepted answers 409 with reason invite_used: remove the member instead.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{}, error) {
		if _, err := h.access.Account(ctx); err != nil {
			return nil, err
		}
		invite, err := h.accounts.GetOrgInvite(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		if err := h.access.Invite(ctx, invite); err != nil {
			return nil, err
		}
		if err := h.accounts.RevokeOrgInvite(ctx, in.ID); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})

	route(doc, h, huma.Operation{
		OperationID: "accept-org-invite", Method: "POST", Path: "/invites/accept", DefaultStatus: http.StatusCreated,
		Summary: "Accept an invite to an org", Tags: []string{"accounts"},
		Description: "For a signed-in person, who joins the invite's org with the invite's role. An API key is no person and is refused. " +
			"A token the platform does not know answers 404. One that cannot be accepted answers 409 with the reason: invite_used, invite_revoked, " +
			"invite_expired, or already_member when the person is in the org already, which leaves the invite unused.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Token string `json:"token"`
		}
	}) (*struct{ Body domain.OrgMember }, error) {
		p, err := h.access.Person(ctx)
		if err != nil {
			return nil, err
		}
		member, err := h.accounts.AcceptOrgInvite(ctx, auth.HashAPIKey(in.Body.Token), p.UserID)
		if err != nil {
			return nil, err
		}
		return &struct{ Body domain.OrgMember }{Body: *member}, nil
	})
}
