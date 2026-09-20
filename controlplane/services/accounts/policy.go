package accounts

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// policy is the one place that says who may do what to orgs, projects, API keys and clusters.
//
//   - staff may do everything.
//   - a signed-in person reads the orgs they belong to and administers the ones they own.
//   - an API key of a whole org administers that org as an owner would, with two limits: it
//     cannot create another org, and of the org's keys it reaches only its own lineage.
//   - an API key bound to one project reads that project and nothing else of the org.
//   - a key mints keys no wider than itself, and reaches itself and the keys minted from it.
//   - every token the platform derives for something it launched is refused.
//
// An org or project outside the caller's reach is answered as not found; one inside it that the
// caller may not change is answered as forbidden.
type policy struct {
	roles RoleReader
}

// RoleReader reads a person's role in an org. *db.AccountsStore satisfies it.
type RoleReader interface {
	OrgRole(ctx context.Context, orgID, userID string) (role domain.OrgRole, found bool, err error)
}

// caller returns the request's principal when it is a person, an API key or staff.
func (policy) caller(ctx context.Context) (auth.Principal, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return auth.Principal{}, auth.Unauthenticated("the request carries no principal")
	}
	switch p.Kind {
	case auth.KindUser, auth.KindKey, auth.KindStaff:
		return p, nil
	default:
		return auth.Principal{}, auth.Forbidden("forbidden: a " + string(p.Kind) + " token does not reach accounts")
	}
}

// inOrg admits a caller whose reach includes the org at all.
func (pol policy) inOrg(ctx context.Context, orgID string) (auth.Principal, error) {
	p, err := pol.caller(ctx)
	if err != nil {
		return p, err
	}
	if !p.Scope().AdmitsOrg(orgID) {
		return p, huma.Error404NotFound("org not found")
	}
	return p, nil
}

// readOrg admits the caller to see what belongs to the org as a whole: its clusters.
func (pol policy) readOrg(ctx context.Context, orgID string) (auth.Principal, error) {
	p, err := pol.inOrg(ctx, orgID)
	if err != nil {
		return p, err
	}
	if p.Kind == auth.KindKey && p.ProjectID != "" {
		return p, auth.Forbidden("forbidden: an API key bound to one project reads that project only")
	}
	return p, nil
}

// administerOrg admits the caller to change an org: rename it, add projects and clusters.
func (pol policy) administerOrg(ctx context.Context, orgID string) (auth.Principal, error) {
	p, err := pol.inOrg(ctx, orgID)
	if err != nil {
		return p, err
	}
	switch p.Kind {
	case auth.KindStaff:
		return p, nil
	case auth.KindKey:
		if p.ProjectID != "" {
			return p, auth.Forbidden("forbidden: an API key bound to one project cannot change its org")
		}
		return p, nil
	default:
		role, _, err := pol.roles.OrgRole(ctx, orgID, p.UserID)
		if err != nil {
			return p, huma.Error500InternalServerError(err.Error())
		}
		if role != domain.OrgRoleOwner {
			return p, auth.Forbidden("forbidden: only an owner of the org may change it")
		}
		return p, nil
	}
}

// visibleProjects narrows an org's projects to the ones the caller may see: all of them, or for
// a key bound to one project that project.
func (pol policy) visibleProjects(ctx context.Context, orgID string, projects []*domain.Project) ([]*domain.Project, error) {
	p, err := pol.inOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := []*domain.Project{}
	for _, project := range projects {
		if p.Scope().Admits(domain.Tenancy{OrgID: project.OrgID, ProjectID: project.ID}) {
			out = append(out, project)
		}
	}
	return out, nil
}

// createOrg admits the caller to create an org and names its owner. A person owns what they
// create. Staff name the owner, and a staff member who is signed in may leave it to be
// themselves. A key belongs to one org and cannot make another.
func (pol policy) createOrg(ctx context.Context, requestedOwner string) (ownerUserID string, err error) {
	p, err := pol.caller(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case p.Kind == auth.KindKey:
		return "", auth.Forbidden("forbidden: an API key belongs to one org and cannot create another")
	case p.Kind == auth.KindStaff && requestedOwner != "":
		return requestedOwner, nil
	case p.UserID == "":
		return "", huma.Error400BadRequest("owner_user_id is required: this token is no person")
	case requestedOwner != "" && requestedOwner != p.UserID:
		return "", auth.Forbidden("forbidden: you can create an org only for yourself")
	default:
		return p.UserID, nil
	}
}

// mintKey admits the caller to create an API key of orgID, bound to projectID when set, and
// returns the principal that mints it.
func (pol policy) mintKey(ctx context.Context, orgID, projectID string) (auth.Principal, error) {
	p, err := pol.inOrg(ctx, orgID)
	if err != nil {
		return p, err
	}
	if p.Kind == auth.KindKey {
		if p.ProjectID != "" && p.ProjectID != projectID {
			return p, auth.Forbidden("forbidden: an API key bound to one project mints keys for that project only")
		}
		return p, nil
	}
	if p, err = pol.administerOrg(ctx, orgID); err != nil {
		return p, err
	}
	if p.UserID == "" {
		return p, auth.Forbidden("forbidden: an API key is created by a signed-in person or minted from another key")
	}
	return p, nil
}

// reachableKeys narrows an org's keys to the ones the caller may see and revoke: all of them for
// an owner and for staff, and for a key itself and its lineage.
func (pol policy) reachableKeys(ctx context.Context, orgID string, keys []*domain.APIKey) ([]*domain.APIKey, error) {
	p, err := pol.inOrg(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if p.Kind == auth.KindKey {
		return lineage(keys, p.KeyID), nil
	}
	if _, err := pol.administerOrg(ctx, orgID); err != nil {
		return nil, err
	}
	return keys, nil
}

// lineage returns root and every key minted from it, directly or through another, in the order
// of keys.
func lineage(keys []*domain.APIKey, root string) []*domain.APIKey {
	parent := make(map[string]string, len(keys))
	for _, k := range keys {
		parent[k.ID] = k.MintedBy
	}
	out := []*domain.APIKey{}
	for _, k := range keys {
		// A key is minted from one created before it, so the walk up ends at a key no key minted.
		for id := k.ID; id != ""; id = parent[id] {
			if id == root {
				out = append(out, k)
				break
			}
		}
	}
	return out
}

// staff admits only the platform's own operators.
func (pol policy) staff(ctx context.Context) error {
	p, err := pol.caller(ctx)
	if err != nil {
		return err
	}
	if p.Kind != auth.KindStaff {
		return auth.Forbidden("forbidden: only staff may do this")
	}
	return nil
}
