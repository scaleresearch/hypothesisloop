package auth

import (
	"context"
	"fmt"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// This file is the one place that says who may do what to an org and to the clusters it owns.
//
//   - A person, an API key and staff hold accounts. Every token the platform derives for
//     something it launched (agent, coordinator, job, runtime, launcher) does not.
//   - Whoever reaches an org reads it. An org outside the caller's reach is not found.
//   - An org and what it owns are changed by its administrators: staff, a signed-in owner, and
//     an API key of the whole org. A member and a key bound to one project are refused. What an
//     org owns is its settings and its projects' settings, the repository attached to a project,
//     its members and invites, its API keys and its clusters.
//   - A member reads the org and works in its projects. Of what the org owns a member makes one
//     thing, API keys bound to one project, and ends only the keys they made.
//   - What one account may create is bounded (config accounts.*). Staff, the platform's own
//     operators, are held to no bound, whoever they create for.
//   - A person leaves an org by themselves, whatever their role. An invite is accepted by a
//     signed-in person, for themselves. The store keeps an org from losing its last owner.
//   - A cluster is administered by the administrators of the org that registered it, whatever is
//     done to it: its settings, its identity, placing a job on it by hand, removing it. Only
//     staff share it.
//   - A caller sees the clusters its jobs can be placed on: its orgs' own and the shared ones.
//     Of a cluster another org owns it sees the name and whether it is connected.
//   - Which of those clusters an org's jobs use is the org's own: its administrators turn any of
//     them off and on for its jobs, a shared one of another org included. That decides nothing
//     for any other org.

// Account returns the principal when it is one that holds an account.
func (a *Access) Account(ctx context.Context) (Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, err
	}
	switch p.Kind {
	case KindUser, KindKey, KindStaff:
		return p, nil
	default:
		return Principal{}, forbidden("forbidden: a " + string(p.Kind) + " token does not reach accounts")
	}
}

// OrgMember admits an account holder whose reach includes the org.
func (a *Access) OrgMember(ctx context.Context, orgID string) (Principal, error) {
	p, err := a.Account(ctx)
	if err != nil {
		return p, err
	}
	if !p.Scope().AdmitsOrg(orgID) {
		return p, NotFound("org")
	}
	// A person and a key reach an org through a row that names it. Staff reach every id, so for
	// them the org's existence is checked here.
	if p.Kind == KindStaff {
		org, err := a.placement.GetOrg(ctx, orgID)
		if err != nil {
			return p, err
		}
		if org == nil {
			return p, NotFound("org")
		}
	}
	return p, nil
}

// OrgAdmin admits the caller to change an org and what it owns.
func (a *Access) OrgAdmin(ctx context.Context, orgID string) (Principal, error) {
	p, err := a.OrgMember(ctx, orgID)
	if err != nil {
		return p, err
	}
	return p, a.administers(ctx, p, orgID)
}

// administers is the rule itself, for a principal already known to reach the org.
func (a *Access) administers(ctx context.Context, p Principal, orgID string) error {
	switch p.Kind {
	case KindStaff:
		return nil
	case KindKey:
		if p.ProjectID != "" {
			return forbidden("forbidden: an API key bound to one project cannot change its org")
		}
		return nil
	case KindUser:
		role, _, err := a.placement.OrgRole(ctx, orgID, p.UserID)
		if err != nil {
			return err
		}
		if role != domain.OrgRoleOwner {
			return forbidden("forbidden: only an owner of the org may change it")
		}
		return nil
	default:
		return forbidden("forbidden: a " + string(p.Kind) + " token does not administer an org")
	}
}

// HeldToLimits reports whether what the caller creates counts against the bounds on what one
// account may create.
func (a *Access) HeldToLimits(ctx context.Context) (bool, error) {
	p, err := a.Account(ctx)
	return p.Kind != KindStaff, err
}

// Person returns the principal when a signed-in person is behind it: a user, or staff signed in
// as one.
func (a *Access) Person(ctx context.Context) (Principal, error) {
	p, err := a.Account(ctx)
	if err != nil {
		return p, err
	}
	if p.UserID == "" {
		return p, forbidden("forbidden: only a signed-in person does this, for themselves")
	}
	return p, nil
}

// Inviter admits the caller to invite people into the org and names the person answerable for
// the invite: the one who asks, or the one who created the API key that asks.
func (a *Access) Inviter(ctx context.Context, orgID string) (userID string, err error) {
	p, err := a.OrgAdmin(ctx, orgID)
	if err != nil {
		return "", err
	}
	if p.Kind == KindKey {
		key, err := a.placement.GetAPIKey(ctx, p.KeyID)
		if err != nil {
			return "", err
		}
		if key == nil {
			return "", fmt.Errorf("the calling api key %s has no row", p.KeyID)
		}
		return key.CreatedBy, nil
	}
	if p.UserID == "" {
		return "", forbidden("forbidden: an invite is made by a signed-in person or with an API key of the org")
	}
	return p.UserID, nil
}

// Invite admits the caller to end an invite: an administrator of its org. An invite with no row,
// passed as nil, and one of an org outside the caller's reach are not found alike.
func (a *Access) Invite(ctx context.Context, invite *domain.OrgInvite) error {
	p, err := a.Account(ctx)
	if err != nil {
		return err
	}
	if invite == nil || !p.Scope().AdmitsOrg(invite.OrgID) {
		return NotFound("invite")
	}
	return a.administers(ctx, p, invite.OrgID)
}

// MemberRemoval admits the caller to end userID's membership of the org: an administrator
// anyone's, a person their own.
func (a *Access) MemberRemoval(ctx context.Context, orgID, userID string) error {
	p, err := a.OrgMember(ctx, orgID)
	if err != nil {
		return err
	}
	if p.Kind == KindUser && p.UserID == userID {
		return nil
	}
	return a.administers(ctx, p, orgID)
}

// KeyMinter admits a person to create an API key of the org, bound to projectID when set: an
// administrator any key, a member one bound to a project. What a key may mint is the accounts
// policy's.
func (a *Access) KeyMinter(ctx context.Context, orgID, projectID string) (Principal, error) {
	p, err := a.OrgMember(ctx, orgID)
	if err != nil {
		return p, err
	}
	if p.Kind == KindUser && projectID != "" {
		return p, nil
	}
	return p, a.administers(ctx, p, orgID)
}

// KeyEnd admits a person to end an API key placed by APIKey: an administrator any key of the
// org, a member the keys they made. Which keys a key may end is the accounts policy's.
func (a *Access) KeyEnd(ctx context.Context, key *domain.APIKey) error {
	p, err := a.OrgMember(ctx, key.OrgID)
	if err != nil {
		return err
	}
	if p.Kind == KindUser && p.UserID == key.CreatedBy {
		return nil
	}
	return a.administers(ctx, p, key.OrgID)
}

// Cluster admits the principal to administer one registered cluster by its name alone. A cluster
// outside the caller's reach, and every cluster for a token narrowed to one project or one
// platform experiment, is not found. Staff pass on a name with no row and get the handler's own
// answer for it.
func (a *Access) Cluster(ctx context.Context, name string) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if p.Kind == KindStaff {
		return nil
	}
	cluster, err := a.placement.GetCluster(ctx, name)
	if err != nil {
		return err
	}
	scope := p.Scope()
	if cluster == nil || !scope.AdmitsOrg(cluster.OrgID) || scope.ProjectID != "" || scope.PlatformExperimentID != "" {
		return NotFound("cluster")
	}
	return a.administers(ctx, p, cluster.OrgID)
}

// ClusterByID admits the principal to administer the registered cluster bound to the id its
// runtime reported, which is the key its settings are kept under. An id that could not be one,
// an id bound to no cluster and a cluster outside the caller's reach are all not found, for staff
// too: settings exist only under an id the platform bound, never under one a request states.
func (a *Access) ClusterByID(ctx context.Context, reportedID string) (*domain.Cluster, error) {
	p, cluster, err := a.clusterByID(ctx, reportedID)
	if err != nil || p.Kind == KindStaff {
		return cluster, err
	}
	return cluster, a.administers(ctx, p, cluster.OrgID)
}

// ReadClusterByID admits the principal to read the settings of that cluster: whoever reads its
// org as a whole, so a member too. What is not found is what ClusterByID does not find.
func (a *Access) ReadClusterByID(ctx context.Context, reportedID string) (*domain.Cluster, error) {
	p, cluster, err := a.clusterByID(ctx, reportedID)
	if err != nil {
		return nil, err
	}
	switch p.Kind {
	case KindUser, KindKey, KindStaff:
		return cluster, nil
	default:
		return nil, forbidden("forbidden: a " + string(p.Kind) + " token does not read a cluster's settings")
	}
}

func (a *Access) clusterByID(ctx context.Context, reportedID string) (Principal, *domain.Cluster, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, nil, err
	}
	if domain.ValidateClusterID(reportedID) != nil {
		return p, nil, NotFound("cluster")
	}
	cluster, err := a.placement.ClusterByReportedID(ctx, reportedID)
	if err != nil {
		return p, nil, err
	}
	if cluster == nil {
		return p, nil, NotFound("cluster")
	}
	if p.Kind == KindStaff {
		return p, cluster, nil
	}
	scope := p.Scope()
	if !scope.AdmitsOrg(cluster.OrgID) || scope.ProjectID != "" || scope.PlatformExperimentID != "" {
		return p, nil, NotFound("cluster")
	}
	return p, cluster, nil
}

// ProjectAdmin admits the caller to change one project, and returns it. A project the caller
// does not see, whether it is another org's, a sibling of the one its key is bound to, or
// absent, is not found before anything else is read.
func (a *Access) ProjectAdmin(ctx context.Context, projectID string) (*domain.Project, error) {
	p, err := a.Account(ctx)
	if err != nil {
		return nil, err
	}
	project, err := a.placement.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if project == nil || !p.Scope().Admits(domain.Tenancy{OrgID: project.OrgID, ProjectID: project.ID}) {
		return nil, NotFound("project")
	}
	return project, a.administers(ctx, p, project.OrgID)
}

// APIKey places one API key for an account holder, and returns it. A key with no row and a key
// of an org or project outside the caller's reach are not found. Which keys of its org a caller
// may end is the accounts policy's.
func (a *Access) APIKey(ctx context.Context, keyID string) (*domain.APIKey, error) {
	p, err := a.Account(ctx)
	if err != nil {
		return nil, err
	}
	key, err := a.placement.GetAPIKey(ctx, keyID)
	if err != nil {
		return nil, err
	}
	if key == nil || !p.Scope().AdmitsOrg(key.OrgID) || (p.ProjectID != "" && p.ProjectID != key.ProjectID) {
		return nil, NotFound("api key")
	}
	return key, nil
}

// ClusterOf admits the principal to administer a cluster named under its org, and returns it.
// A cluster of another org is not found under this one.
func (a *Access) ClusterOf(ctx context.Context, orgID, name string) (*domain.Cluster, error) {
	if _, err := a.OrgAdmin(ctx, orgID); err != nil {
		return nil, err
	}
	cluster, err := a.placement.GetCluster(ctx, name)
	if err != nil {
		return nil, err
	}
	if cluster == nil || cluster.OrgID != orgID {
		return nil, NotFound("cluster")
	}
	return cluster, nil
}

// ClusterUse admits the principal to decide whether the jobs of orgID use a cluster, and returns
// both: an administrator of the org, for a cluster open to it, its own or a shared one. Any
// other cluster is not found under this org.
func (a *Access) ClusterUse(ctx context.Context, orgID, name string) (Principal, *domain.Cluster, error) {
	p, err := a.OrgAdmin(ctx, orgID)
	if err != nil {
		return p, nil, err
	}
	cluster, err := a.placement.GetCluster(ctx, name)
	if err != nil {
		return p, nil, err
	}
	if cluster == nil || !cluster.OpenTo(orgID) {
		return p, nil, NotFound("cluster")
	}
	return p, cluster, nil
}

// Actor names the account behind a decision that is recorded with its author: the person, the
// API key, or staff acting with no person signed in.
func (p Principal) Actor() string {
	switch {
	case p.UserID != "":
		return p.UserID
	case p.KeyID != "":
		return p.KeyID
	default:
		return string(p.Kind)
	}
}

// PlacementOrg resolves the org whose clusters a caller reads the capacity of. What an org can
// be placed on is that org's to know, so a caller who reaches several orgs reads one at a time.
//
//   - a caller bound to one org (every key, agent and coordinator, and a person in one org) reads
//     that org, named or not.
//   - a person in several orgs names the one they mean.
//   - an org outside the caller's reach, and for staff one that does not exist, is not found.
//   - staff who name no org read every cluster: every is true and orgID empty.
func (a *Access) PlacementOrg(ctx context.Context, requested string) (orgID string, every bool, err error) {
	p, err := principal(ctx)
	if err != nil {
		return "", false, err
	}
	if p.Kind != KindStaff {
		orgID, err = a.Org(ctx, requested)
		return orgID, false, err
	}
	if requested == "" {
		return "", true, nil
	}
	org, err := a.placement.GetOrg(ctx, requested)
	if err != nil {
		return "", false, err
	}
	if org == nil {
		return "", false, NotFound("org")
	}
	return requested, false, nil
}

// KnowsCluster reports whether the principal is shown who owns the cluster and the identity its
// settings are kept under: staff, and callers of the org that registered it.
func (p Principal) KnowsCluster(cluster domain.Cluster) bool {
	return p.Scope().AdmitsOrg(cluster.OrgID)
}
