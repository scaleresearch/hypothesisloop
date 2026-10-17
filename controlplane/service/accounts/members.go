package accounts

import (
	"context"
	"fmt"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
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
		return 0, 0, refusal.Invalid(ReasonInvalidPage, fmt.Sprintf("limit must be 1 to %d and offset must not be negative", maxPageLimit))
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

func validRole(role domain.OrgRole) error {
	if !domain.ValidOrgRole(role) {
		return refusal.Invalid(ReasonInvalidRole, "role must be owner or member")
	}
	return nil
}

// Members is one page of the members of orgID, and how many it has.
func (h *Service) Members(ctx context.Context, orgID string, page Page) ([]*domain.OrgMember, int, error) {
	if _, err := h.policy.readOrg(ctx, orgID); err != nil {
		return nil, 0, err
	}
	limit, offset, err := page.bounds()
	if err != nil {
		return nil, 0, err
	}
	return h.accounts.ListOrgMembers(ctx, orgID, limit, offset)
}

func (h *Service) SetMemberRole(ctx context.Context, orgID, userID string, role domain.OrgRole) (*domain.OrgMember, error) {
	if _, err := h.access.OrgAdmin(ctx, orgID); err != nil {
		return nil, err
	}
	if err := validRole(role); err != nil {
		return nil, err
	}
	return h.accounts.SetOrgMemberRole(ctx, orgID, userID, role)
}

func (h *Service) RemoveMember(ctx context.Context, orgID, userID string) error {
	if err := h.access.MemberRemoval(ctx, orgID, userID); err != nil {
		return err
	}
	return h.accounts.RemoveOrgMember(ctx, orgID, userID)
}

func (h *Service) CreateInvite(ctx context.Context, orgID string, role domain.OrgRole) (*CreatedInvite, error) {
	createdBy, err := h.access.Inviter(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if err := validRole(role); err != nil {
		return nil, err
	}
	id, token, err := auth.NewInvite()
	if err != nil {
		return nil, err
	}
	invite := &domain.OrgInvite{ID: id, OrgID: orgID, Role: role, Actor: domain.Actor{Kind: domain.ActorUser, ID: createdBy}, ExpiresAt: time.Now().UTC().Add(domain.InviteLifetime)}
	if err := h.accounts.CreateOrgInvite(ctx, invite, auth.HashAPIKey(token)); err != nil {
		return nil, err
	}
	return &CreatedInvite{OrgInvite: *invite, Token: token}, nil
}

// Invites is one page of the pending invites of orgID, and how many there are.
func (h *Service) Invites(ctx context.Context, orgID string, page Page) ([]*domain.OrgInvite, int, error) {
	if _, err := h.access.OrgAdmin(ctx, orgID); err != nil {
		return nil, 0, err
	}
	limit, offset, err := page.bounds()
	if err != nil {
		return nil, 0, err
	}
	return h.accounts.ListPendingOrgInvites(ctx, orgID, limit, offset)
}

func (h *Service) RevokeInvite(ctx context.Context, id string) error {
	if _, err := h.access.Account(ctx); err != nil {
		return err
	}
	invite, err := h.accounts.GetOrgInvite(ctx, id)
	if err != nil {
		return err
	}
	if err := h.access.Invite(ctx, invite); err != nil {
		return err
	}
	return h.accounts.RevokeOrgInvite(ctx, id)
}

// AcceptInvite joins the signed-in caller to the org of the invite token names.
func (h *Service) AcceptInvite(ctx context.Context, token string) (*domain.OrgMember, error) {
	p, err := h.access.Person(ctx)
	if err != nil {
		return nil, err
	}
	return h.accounts.AcceptOrgInvite(ctx, auth.HashAPIKey(token), p.UserID)
}
