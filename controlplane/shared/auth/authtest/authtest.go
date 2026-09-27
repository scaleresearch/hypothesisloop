// Package authtest gives handler tests a caller without a signing secret or a database.
package authtest

import (
	"context"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// As authenticates every token as one fixed principal.
type As auth.Principal

func (a As) Authenticate(context.Context, string) (auth.Principal, error) {
	return auth.Principal(a), nil
}

// Staff authenticates every token as staff.
var Staff = As{Kind: auth.KindStaff}

// Authorize gives a request the bearer token As accepts.
func Authorize(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer test")
	return r
}

// Placement is an in-memory auth.Placement.
type Placement struct {
	PlatformExperiments map[string]domain.Tenancy
	Experiments         map[string]domain.Tenancy
	Agents              map[string]string
	Clusters            map[string]string
	Projects            map[string]domain.Project
	Keys                map[string]domain.APIKey
	// Members holds the users of an org who are not its owners, as "org/user". Every other user
	// asked about owns the org, which is the only kind of person production has.
	Members map[string]bool
}

func (p Placement) PlatformExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, *time.Time, error) {
	return lookup(p.PlatformExperiments, id), nil, nil
}

func (p Placement) ExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, bool, error) {
	return lookup(p.Experiments, id), false, nil
}

func (p Placement) AgentOrg(_ context.Context, id string) (string, bool, error) {
	org, ok := p.Agents[id]
	return org, ok, nil
}

func (p Placement) GetAPIKey(_ context.Context, id string) (*domain.APIKey, error) {
	key, ok := p.Keys[id]
	if !ok {
		return nil, nil
	}
	return &key, nil
}

func (p Placement) GetCluster(_ context.Context, name string) (*domain.Cluster, error) {
	org, ok := p.Clusters[name]
	if !ok {
		return nil, nil
	}
	return &domain.Cluster{Name: name, OrgID: org}, nil
}

// GetOrg knows every org a cluster or a project of the placement names.
func (p Placement) GetOrg(_ context.Context, id string) (*domain.Org, error) {
	for _, org := range p.Clusters {
		if org == id {
			return &domain.Org{ID: id}, nil
		}
	}
	for _, project := range p.Projects {
		if project.OrgID == id {
			return &domain.Org{ID: id}, nil
		}
	}
	return nil, nil
}

func (p Placement) OrgRole(_ context.Context, orgID, userID string) (domain.OrgRole, bool, error) {
	if p.Members[orgID+"/"+userID] {
		return domain.OrgRoleMember, true, nil
	}
	return domain.OrgRoleOwner, true, nil
}

func (p Placement) GetProject(_ context.Context, id string) (*domain.Project, error) {
	project, ok := p.Projects[id]
	if !ok {
		return nil, nil
	}
	return &project, nil
}

func (p Placement) PlatformExperimentIDs(_ context.Context, scope domain.TenantScope) ([]string, error) {
	ids := []string{}
	for id, t := range p.PlatformExperiments {
		if scope.Admits(t) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func lookup(m map[string]domain.Tenancy, id string) *domain.Tenancy {
	t, ok := m[id]
	if !ok {
		return nil
	}
	return &t
}
