package accounts

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// policy says what an API key reaches among projects and keys. Who reads and who administers an
// org, and what a member may do in it, is auth.Access's (shared/auth/orgs.go), which every rule
// here starts from.
//
//   - an API key bound to one project reads that project and nothing else of the org.
//   - an API key cannot create an org: it belongs to one.
//   - a key mints keys no wider than itself, and reaches itself and the keys minted from it.
//
// An org or project outside the caller's reach is answered as not found; one inside it that the
// caller may not change is answered as forbidden.
type policy struct {
	access *auth.Access
}

// readOrg admits the caller to see what belongs to the org as a whole: the clusters it runs on.
func (pol policy) readOrg(ctx context.Context, orgID string) (auth.Principal, error) {
	p, err := pol.access.OrgMember(ctx, orgID)
	if err != nil {
		return p, err
	}
	if p.Kind == auth.KindKey && p.ProjectID != "" {
		return p, auth.Forbidden("forbidden: an API key bound to one project reads that project only")
	}
	return p, nil
}

// visibleProjects narrows an org's projects to the ones the caller may see: all of them, or for
// a key bound to one project that project.
func (pol policy) visibleProjects(ctx context.Context, orgID string, projects []*domain.Project) ([]*domain.Project, error) {
	p, err := pol.access.OrgMember(ctx, orgID)
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

// changeProject admits the caller to change one project. A project the caller does not see,
// whether it is another org's, a sibling of the one its key is bound to, or absent, is not found.
func (pol policy) changeProject(ctx context.Context, project *domain.Project) error {
	if err := pol.readProject(ctx, project); err != nil {
		return err
	}
	_, err := pol.access.OrgAdmin(ctx, project.OrgID)
	return err
}

// readProject admits an account holder to a project it sees, under the same rule of what reads
// as not found.
func (pol policy) readProject(ctx context.Context, project *domain.Project) error {
	p, err := pol.access.Account(ctx)
	if err != nil {
		return err
	}
	if project == nil || !p.Scope().Admits(domain.Tenancy{OrgID: project.OrgID, ProjectID: project.ID}) {
		return auth.NotFound("project")
	}
	return nil
}

// createOrg admits the caller to create an org and names its owner. A person owns what they
// create. Staff name the owner, and a staff member who is signed in may leave it to be
// themselves. A key belongs to one org and cannot make another.
func (pol policy) createOrg(ctx context.Context, requestedOwner string) (ownerUserID string, err error) {
	p, err := pol.access.Account(ctx)
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
	p, err := pol.access.OrgMember(ctx, orgID)
	if err != nil {
		return p, err
	}
	if p.Kind == auth.KindKey {
		if p.ProjectID != "" && p.ProjectID != projectID {
			return p, auth.Forbidden("forbidden: an API key bound to one project mints keys for that project only")
		}
		return p, nil
	}
	if p, err = pol.access.KeyMinter(ctx, orgID, projectID); err != nil {
		return p, err
	}
	if p.UserID == "" {
		return p, auth.Forbidden("forbidden: an API key is created by a signed-in person or minted from another key")
	}
	return p, nil
}

// reachableKeys narrows an org's keys to the ones the caller may see: all of them for a person
// of the org and for staff, and for a key itself and its lineage.
func (pol policy) reachableKeys(ctx context.Context, orgID string, keys []*domain.APIKey) ([]*domain.APIKey, error) {
	p, err := pol.access.OrgMember(ctx, orgID)
	if err != nil {
		return nil, err
	}
	if p.Kind == auth.KindKey {
		return lineage(keys, p.KeyID), nil
	}
	return keys, nil
}

// endKey admits the caller to end a key it was placed at. A key ends itself and the keys minted
// from it, read from orgKeys, and any other is not found for it. A person is held to
// auth.Access.KeyEnd.
func (pol policy) endKey(ctx context.Context, key *domain.APIKey, orgKeys func() ([]*domain.APIKey, error)) error {
	p, err := pol.access.Account(ctx)
	if err != nil {
		return err
	}
	if p.Kind != auth.KindKey {
		return pol.access.KeyEnd(ctx, key)
	}
	keys, err := orgKeys()
	if err != nil {
		return err
	}
	for _, k := range lineage(keys, p.KeyID) {
		if k.ID == key.ID {
			return nil
		}
	}
	return auth.NotFound("api key")
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
