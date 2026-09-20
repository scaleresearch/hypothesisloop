package auth

import (
	"context"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// This file is the one place that says who may do what to an org and to the clusters it owns.
//
//   - A person, an API key and staff hold accounts. Every token the platform derives for
//     something it launched (agent, coordinator, job, runtime, launcher) does not.
//   - Whoever reaches an org reads it. An org outside the caller's reach is not found.
//   - An org and what it owns are changed by its administrators: staff, a signed-in owner, and
//     an API key of the whole org. A member and a key bound to one project are refused.
//   - A cluster is administered by the administrators of the org that registered it, whatever is
//     done to it: its settings, its identity, placing a job on it by hand, removing it. Only
//     staff share it.
//   - A caller sees the clusters its jobs can be placed on: its orgs' own and the shared ones.
//     Of a cluster another org owns it sees the name and whether it is connected.

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
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if domain.ValidateClusterID(reportedID) != nil {
		return nil, NotFound("cluster")
	}
	cluster, err := a.placement.ClusterByReportedID(ctx, reportedID)
	if err != nil {
		return nil, err
	}
	if cluster == nil {
		return nil, NotFound("cluster")
	}
	if p.Kind == KindStaff {
		return cluster, nil
	}
	scope := p.Scope()
	if !scope.AdmitsOrg(cluster.OrgID) || scope.ProjectID != "" || scope.PlatformExperimentID != "" {
		return nil, NotFound("cluster")
	}
	return cluster, a.administers(ctx, p, cluster.OrgID)
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
