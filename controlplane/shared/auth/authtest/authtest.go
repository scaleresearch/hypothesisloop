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
	Donations           map[string]domain.Tenancy
	Agents              map[string]string
	Clusters            map[string]string
	Projects            map[string]domain.Project
}

func (p Placement) PlatformExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, *time.Time, error) {
	return lookup(p.PlatformExperiments, id), nil, nil
}

func (p Placement) ExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, bool, error) {
	return lookup(p.Experiments, id), false, nil
}

func (p Placement) DonationTenancy(_ context.Context, id string) (*domain.Tenancy, error) {
	return lookup(p.Donations, id), nil
}

func (p Placement) AgentOrg(_ context.Context, id string) (string, bool, error) {
	org, ok := p.Agents[id]
	return org, ok, nil
}

func (p Placement) ClusterOrg(_ context.Context, name string) (string, bool, error) {
	org, ok := p.Clusters[name]
	return org, ok, nil
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
