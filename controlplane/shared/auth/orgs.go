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

// PlacedOn reports whether the principal's jobs can be placed on the cluster, which is what
// makes the cluster visible to it.
func (p Principal) PlacedOn(cluster domain.Cluster) bool {
	if p.Kind == KindStaff {
		return true
	}
	for _, orgID := range p.OrgIDs {
		if cluster.Hosts(orgID) {
			return true
		}
	}
	return false
}

// KnowsCluster reports whether the principal is shown who owns the cluster and the identity its
// settings are kept under: staff, and callers of the org that registered it.
func (p Principal) KnowsCluster(cluster domain.Cluster) bool {
	return p.Scope().AdmitsOrg(cluster.OrgID)
}
