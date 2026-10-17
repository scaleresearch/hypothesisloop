package accounts

import (
	"context"
	"slices"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// policy says what an API key reaches among projects and keys. Who reads and who administers an
// org, and what a member may do in it, is auth.Access's (auth/orgs.go), which every rule
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
		return p, auth.Forbidden("an API key bound to one project reads that project only")
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
		return "", auth.Forbidden("an API key belongs to one org and cannot create another")
	case p.Kind == auth.KindStaff && requestedOwner != "":
		return requestedOwner, nil
	case p.UserID == "":
		return "", refusal.Invalid(refusal.ReasonInvalidRequest, "owner_user_id is required: this token is no person").On("owner_user_id")
	case requestedOwner != "" && requestedOwner != p.UserID:
		return "", auth.Forbidden("you can create an org only for yourself")
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
			return p, auth.Forbidden("an API key bound to one project mints keys for that project only")
		}
		return p, nil
	}
	if p, err = pol.access.KeyMinter(ctx, orgID, projectID); err != nil {
		return p, err
	}
	if !mintsKeys(p) {
		return p, auth.Forbidden("an API key is created by a signed-in person or minted from another key")
	}
	return p, nil
}

// mintsKeys says p makes API keys at all: a signed-in person, or a key minting from itself.
func mintsKeys(p auth.Principal) bool {
	return p.Kind == auth.KindKey || p.UserID != ""
}

// orgActions is what p may do to org, in which it holds org.Role, by the rules the writes refuse
// by: change it and what it owns, and create API keys, a member only ones bound to a project.
func orgActions(p auth.Principal, org *domain.Org) []domain.OrgAction {
	actions := []domain.OrgAction{}
	if auth.Administers(p, org.Role) == nil {
		actions = append(actions, domain.OrgActionEdit)
	}
	if mintsKeys(p) {
		actions = append(actions, domain.OrgActionCreateAPIKey)
	}
	return actions
}

// reachableKeys narrows an org's keys to the ones p may see: all of them for a person of the org
// and for staff, and for a key itself and its lineage.
func reachableKeys(p auth.Principal, keys []*domain.APIKey) []*domain.APIKey {
	if p.Kind == auth.KindKey {
		return lineage(keys, p.KeyID)
	}
	return keys
}

// endKey admits the caller to end a key it was placed at, by endsKey: a key it may not end is
// not found for a key, and refused by auth.Access.KeyEnd for a person.
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
	if !endsKey(p, "", key, keys) {
		return auth.NotFound("api key")
	}
	return nil
}

// endsKey says p, holding role in the org of key, ends it: a key ends itself and the keys minted
// from it, of the org's keys orgKeys, and a person is held to auth.EndsKey.
func endsKey(p auth.Principal, role domain.OrgRole, key *domain.APIKey, orgKeys []*domain.APIKey) bool {
	if p.Kind != auth.KindKey {
		return auth.EndsKey(p, role, key) == nil
	}
	return slices.ContainsFunc(lineage(orgKeys, p.KeyID), func(k *domain.APIKey) bool { return k.ID == key.ID })
}

// keyActions is what p, holding role in the org, may do to key, one of the org's keys orgKeys.
// Revoking a revoked key changes nothing, so it is not offered.
func keyActions(p auth.Principal, role domain.OrgRole, key *domain.APIKey, orgKeys []*domain.APIKey) []domain.APIKeyAction {
	actions := []domain.APIKeyAction{}
	if key.RevokedAt == nil && endsKey(p, role, key, orgKeys) {
		actions = append(actions, domain.APIKeyActionRevoke)
	}
	return actions
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
