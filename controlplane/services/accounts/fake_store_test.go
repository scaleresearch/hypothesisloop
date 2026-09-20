package accounts

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// memStore is Accounts and Clusters in memory, refusing what the database refuses.
type memStore struct {
	clock    time.Time
	sequence int
	users    map[string]bool
	orgs     map[string]*domain.Org
	roles    map[string]map[string]domain.OrgRole // org -> user -> role
	projects map[string]*domain.Project
	keys     map[string]*domain.APIKey
	hashes   map[string]string // key id -> hash
	clusters map[string]*domain.Cluster
}

func newMemStore() *memStore {
	return &memStore{
		clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		users: map[string]bool{}, orgs: map[string]*domain.Org{}, roles: map[string]map[string]domain.OrgRole{},
		projects: map[string]*domain.Project{}, keys: map[string]*domain.APIKey{}, hashes: map[string]string{},
		clusters: map[string]*domain.Cluster{},
	}
}

func (s *memStore) now() time.Time {
	s.clock = s.clock.Add(time.Second)
	return s.clock
}

func (s *memStore) id(prefix string) string {
	s.sequence++
	return fmt.Sprintf("%s-%d", prefix, s.sequence)
}

func (s *memStore) OrgRole(_ context.Context, orgID, userID string) (domain.OrgRole, bool, error) {
	role, ok := s.roles[orgID][userID]
	return role, ok, nil
}

func (s *memStore) CreateOrg(_ context.Context, name, ownerUserID string) (*domain.Org, error) {
	if !s.users[ownerUserID] {
		return nil, db.ErrUserNotFound
	}
	org := &domain.Org{ID: s.id("org"), Name: name, CreatedAt: s.now()}
	s.orgs[org.ID] = org
	s.roles[org.ID] = map[string]domain.OrgRole{ownerUserID: domain.OrgRoleOwner}
	projectID := s.id("prj")
	s.projects[projectID] = &domain.Project{ID: projectID, OrgID: org.ID, Name: domain.DefaultProjectName, CreatedAt: s.now()}
	owned := *org
	owned.Role = domain.OrgRoleOwner
	return &owned, nil
}

func (s *memStore) RenameOrg(_ context.Context, id, name string) error {
	org, ok := s.orgs[id]
	if !ok {
		return db.ErrOrgNotFound
	}
	org.Name = name
	return nil
}

func (s *memStore) GetOrg(_ context.Context, id string) (*domain.Org, error) {
	org, ok := s.orgs[id]
	if !ok {
		return nil, nil
	}
	out := *org
	return &out, nil
}

func (s *memStore) listOrgs(keep func(*domain.Org) bool) []*domain.Org {
	out := []*domain.Org{}
	for _, org := range s.orgs {
		if keep(org) {
			copied := *org
			out = append(out, &copied)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (s *memStore) ListOrgs(_ context.Context, orgIDs []string) ([]*domain.Org, error) {
	return s.listOrgs(func(org *domain.Org) bool { return contains(orgIDs, org.ID) }), nil
}

func (s *memStore) ListAllOrgs(context.Context) ([]*domain.Org, error) {
	return s.listOrgs(func(*domain.Org) bool { return true }), nil
}

func (s *memStore) ListUserOrgs(_ context.Context, userID string) ([]*domain.Org, error) {
	out := s.listOrgs(func(org *domain.Org) bool { _, ok := s.roles[org.ID][userID]; return ok })
	for _, org := range out {
		org.Role = s.roles[org.ID][userID]
	}
	return out, nil
}

func (s *memStore) projectNameTaken(orgID, name, exceptID string) bool {
	for _, p := range s.projects {
		if p.OrgID == orgID && p.Name == name && p.ID != exceptID {
			return true
		}
	}
	return false
}

func (s *memStore) CreateProject(_ context.Context, orgID, name string) (*domain.Project, error) {
	if _, ok := s.orgs[orgID]; !ok {
		return nil, db.ErrOrgNotFound
	}
	if s.projectNameTaken(orgID, name, "") {
		return nil, db.ErrProjectNameTaken
	}
	p := &domain.Project{ID: s.id("prj"), OrgID: orgID, Name: name, CreatedAt: s.now()}
	s.projects[p.ID] = p
	out := *p
	return &out, nil
}

func (s *memStore) RenameProject(_ context.Context, id, name string) error {
	p, ok := s.projects[id]
	if !ok {
		return db.ErrProjectNotFound
	}
	if s.projectNameTaken(p.OrgID, name, id) {
		return db.ErrProjectNameTaken
	}
	p.Name = name
	return nil
}

func (s *memStore) GetProject(_ context.Context, id string) (*domain.Project, error) {
	p, ok := s.projects[id]
	if !ok {
		return nil, nil
	}
	out := *p
	return &out, nil
}

func (s *memStore) ListProjects(_ context.Context, orgIDs []string) ([]*domain.Project, error) {
	out := []*domain.Project{}
	for _, p := range s.projects {
		if contains(orgIDs, p.OrgID) {
			copied := *p
			out = append(out, &copied)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *memStore) CreateAPIKey(_ context.Context, key *domain.APIKey, keyHash string) error {
	if !s.users[key.CreatedBy] {
		return fmt.Errorf("created_by %q is no user", key.CreatedBy)
	}
	if key.ProjectID != "" {
		if p, ok := s.projects[key.ProjectID]; !ok || p.OrgID != key.OrgID {
			return db.ErrProjectNotFound
		}
	}
	key.CreatedAt = s.now()
	stored := *key
	s.keys[key.ID] = &stored
	s.hashes[key.ID] = keyHash
	return nil
}

func (s *memStore) ListAPIKeys(_ context.Context, orgID string) ([]*domain.APIKey, error) {
	out := []*domain.APIKey{}
	for _, k := range s.keys {
		if k.OrgID == orgID {
			copied := *k
			out = append(out, &copied)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *memStore) GetAPIKey(_ context.Context, id string) (*domain.APIKey, error) {
	k, ok := s.keys[id]
	if !ok {
		return nil, nil
	}
	out := *k
	return &out, nil
}

func (s *memStore) RevokeAPIKey(_ context.Context, id string) error {
	if _, ok := s.keys[id]; !ok {
		return db.ErrAPIKeyNotFound
	}
	now := s.now()
	for _, k := range lineage(s.allKeys(), id) {
		if stored := s.keys[k.ID]; stored.RevokedAt == nil {
			stored.RevokedAt = &now
		}
	}
	return nil
}

func (s *memStore) allKeys() []*domain.APIKey {
	out := make([]*domain.APIKey, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k)
	}
	return out
}

func (s *memStore) RegisterCluster(_ context.Context, name, orgID string) (*domain.Cluster, error) {
	if _, ok := s.orgs[orgID]; !ok {
		return nil, db.ErrOrgNotFound
	}
	c, ok := s.clusters[name]
	if !ok {
		c = &domain.Cluster{Name: name, OrgID: orgID, CreatedAt: s.now()}
		s.clusters[name] = c
	}
	if c.OrgID != orgID {
		return nil, db.ErrClusterNameTaken
	}
	out := *c
	return &out, nil
}

func (s *memStore) GetCluster(_ context.Context, name string) (*domain.Cluster, error) {
	c, ok := s.clusters[name]
	if !ok {
		return nil, nil
	}
	out := *c
	return &out, nil
}

func (s *memStore) ClustersHosting(_ context.Context, orgID string) ([]*domain.Cluster, error) {
	out := []*domain.Cluster{}
	for _, c := range s.clusters {
		if c.Hosts(orgID) {
			copied := *c
			out = append(out, &copied)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *memStore) SetClusterShared(_ context.Context, name string, shared bool) (*domain.Cluster, error) {
	c, ok := s.clusters[name]
	if !ok {
		return nil, db.ErrClusterNotFound
	}
	c.Shared = shared
	out := *c
	return &out, nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
