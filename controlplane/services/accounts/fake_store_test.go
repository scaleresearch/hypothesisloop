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
	staff    map[string]bool
	// placed is the jobs on each cluster, job id -> org; clearedIDs the identities forgotten.
	placed     map[string]map[string]string
	clearedIDs []string
	orgs       map[string]*domain.Org
	roles      map[string]map[string]domain.OrgRole // org -> user -> role
	projects   map[string]*domain.Project
	keys       map[string]*domain.APIKey
	hashes     map[string]string // key id -> hash
	clusters   map[string]*domain.Cluster
	joined     map[string]map[string]time.Time // org -> user -> when
	invites    map[string]*domain.OrgInvite
	inviteIDs  map[string]string // token hash -> invite id
}

func newMemStore() *memStore {
	return &memStore{
		clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		users: map[string]bool{}, staff: map[string]bool{"u-staff": true}, placed: map[string]map[string]string{}, orgs: map[string]*domain.Org{}, roles: map[string]map[string]domain.OrgRole{},
		projects: map[string]*domain.Project{}, keys: map[string]*domain.APIKey{}, hashes: map[string]string{},
		clusters: map[string]*domain.Cluster{}, joined: map[string]map[string]time.Time{},
		invites: map[string]*domain.OrgInvite{}, inviteIDs: map[string]string{},
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

func (s *memStore) CreateOrg(_ context.Context, name, ownerUserID string, maxOwned int) (*domain.Org, error) {
	if !s.users[ownerUserID] {
		return nil, db.ErrUserNotFound
	}
	owns := 0
	for _, roles := range s.roles {
		if roles[ownerUserID] == domain.OrgRoleOwner {
			owns++
		}
	}
	if maxOwned > 0 && owns >= maxOwned {
		return nil, db.ErrMaxOrgsReached
	}
	org := &domain.Org{ID: s.id("org"), Name: name, CreatedAt: s.now()}
	s.orgs[org.ID] = org
	s.roles[org.ID] = map[string]domain.OrgRole{}
	s.join(org.ID, ownerUserID, domain.OrgRoleOwner)
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

func (s *memStore) CreateProject(_ context.Context, orgID, name string, maxProjects int) (*domain.Project, error) {
	if _, ok := s.orgs[orgID]; !ok {
		return nil, db.ErrOrgNotFound
	}
	if s.projectNameTaken(orgID, name, "") {
		return nil, db.ErrProjectNameTaken
	}
	held := 0
	for _, p := range s.projects {
		if p.OrgID == orgID {
			held++
		}
	}
	if maxProjects > 0 && held >= maxProjects {
		return nil, db.ErrMaxProjectsReached
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
	if role, member := s.roles[key.OrgID][key.CreatedBy]; !s.staff[key.CreatedBy] && role != domain.OrgRoleOwner && (!member || key.ProjectID == "") {
		return db.ErrAPIKeyCreatorLacksOrg
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

// asRead returns a key as the store reads it: revoked while any key it descends from is.
func (s *memStore) asRead(k *domain.APIKey) *domain.APIKey {
	out := *k
	for parent := s.keys[k.MintedBy]; parent != nil; parent = s.keys[parent.MintedBy] {
		if parent.RevokedAt != nil && (out.RevokedAt == nil || parent.RevokedAt.Before(*out.RevokedAt)) {
			out.RevokedAt = parent.RevokedAt
		}
	}
	return &out
}

func (s *memStore) ListAPIKeys(_ context.Context, orgID string) ([]*domain.APIKey, error) {
	out := []*domain.APIKey{}
	for _, k := range s.keys {
		if k.OrgID == orgID {
			out = append(out, s.asRead(k))
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
	return s.asRead(k), nil
}

func (s *memStore) RevokeAPIKey(_ context.Context, id string) error {
	stored, ok := s.keys[id]
	if !ok {
		return db.ErrAPIKeyNotFound
	}
	if now := s.now(); stored.RevokedAt == nil {
		stored.RevokedAt = &now
	}
	return nil
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

// placed is what the store finds on a cluster: job id -> org. Withdrawing a cluster requeues
// them as db.ClustersStore does, which the fake records in requeued.
func (s *memStore) withdraw(name, keepOrg string) []string {
	ids := []string{}
	for id, org := range s.placed[name] {
		if org != keepOrg {
			ids = append(ids, id)
			delete(s.placed[name], id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *memStore) SetClusterShared(_ context.Context, name string, shared bool) (*domain.Cluster, []string, error) {
	c, ok := s.clusters[name]
	if !ok {
		return nil, nil, db.ErrClusterNotFound
	}
	c.Shared = shared
	var requeued []string
	if !shared {
		requeued = s.withdraw(name, c.OrgID)
	}
	out := *c
	return &out, requeued, nil
}

func (s *memStore) RemoveCluster(_ context.Context, name string) ([]string, error) {
	if _, ok := s.clusters[name]; !ok {
		return nil, db.ErrClusterNotFound
	}
	requeued := s.withdraw(name, "")
	delete(s.clusters, name)
	return requeued, nil
}

func (s *memStore) ClearReportedID(_ context.Context, name string) (*domain.Cluster, error) {
	c, ok := s.clusters[name]
	if !ok {
		return nil, db.ErrClusterNotFound
	}
	s.clearedIDs = append(s.clearedIDs, c.ReportedID)
	c.ReportedID = ""
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

func (s *memStore) join(orgID, userID string, role domain.OrgRole) *domain.OrgMember {
	if s.joined[orgID] == nil {
		s.joined[orgID] = map[string]time.Time{}
	}
	s.roles[orgID][userID], s.joined[orgID][userID] = role, s.now()
	return &domain.OrgMember{OrgID: orgID, UserID: userID, Role: role, JoinedAt: s.joined[orgID][userID]}
}

func window[T any](all []T, limit, offset int) []T {
	if offset > len(all) {
		offset = len(all)
	}
	return all[offset:min(len(all), offset+limit)]
}

func (s *memStore) ListOrgMembers(_ context.Context, orgID string, limit, offset int) ([]*domain.OrgMember, int, error) {
	out := []*domain.OrgMember{}
	for userID, role := range s.roles[orgID] {
		out = append(out, &domain.OrgMember{OrgID: orgID, UserID: userID, Role: role, JoinedAt: s.joined[orgID][userID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JoinedAt.Before(out[j].JoinedAt) })
	return window(out, limit, offset), len(out), nil
}

// leaveOwnership is what the database does when a member stops being an owner: it refuses to
// leave the org with none, and revokes the invites the member made.
func (s *memStore) leaveOwnership(orgID, userID string, change func()) error {
	if _, ok := s.roles[orgID][userID]; !ok {
		return db.ErrOrgMemberNotFound
	}
	for other, role := range s.roles[orgID] {
		if other != userID && role == domain.OrgRoleOwner {
			change()
			for _, invite := range s.invites {
				if invite.OrgID == orgID && invite.CreatedBy == userID && invite.AcceptedAt == nil && invite.RevokedAt == nil {
					now := s.now()
					invite.RevokedAt = &now
				}
			}
			return nil
		}
	}
	return db.ErrLastOwner
}

func (s *memStore) SetOrgMemberRole(_ context.Context, orgID, userID string, role domain.OrgRole) (*domain.OrgMember, error) {
	if _, ok := s.roles[orgID][userID]; !ok {
		return nil, db.ErrOrgMemberNotFound
	}
	if role == domain.OrgRoleOwner {
		s.roles[orgID][userID] = role
	} else if err := s.leaveOwnership(orgID, userID, func() { s.roles[orgID][userID] = role }); err != nil {
		return nil, err
	}
	return &domain.OrgMember{OrgID: orgID, UserID: userID, Role: role, JoinedAt: s.joined[orgID][userID]}, nil
}

func (s *memStore) RemoveOrgMember(_ context.Context, orgID, userID string) error {
	return s.leaveOwnership(orgID, userID, func() {
		delete(s.roles[orgID], userID)
		delete(s.joined[orgID], userID)
	})
}

func (s *memStore) CreateOrgInvite(_ context.Context, invite *domain.OrgInvite, tokenHash string) error {
	if !s.users[invite.CreatedBy] {
		return fmt.Errorf("created_by %q is no user", invite.CreatedBy)
	}
	invite.CreatedAt = s.now()
	stored := *invite
	s.invites[invite.ID], s.inviteIDs[tokenHash] = &stored, invite.ID
	return nil
}

// The fake's clock is its own, so an invite is expired when the test moved its expiry before it.
func (s *memStore) pending(invite *domain.OrgInvite) bool {
	return invite.AcceptedAt == nil && invite.RevokedAt == nil && invite.ExpiresAt.After(s.clock)
}

func (s *memStore) ListPendingOrgInvites(_ context.Context, orgID string, limit, offset int) ([]*domain.OrgInvite, int, error) {
	out := []*domain.OrgInvite{}
	for _, invite := range s.invites {
		if invite.OrgID == orgID && s.pending(invite) {
			copied := *invite
			out = append(out, &copied)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return window(out, limit, offset), len(out), nil
}

func (s *memStore) GetOrgInvite(_ context.Context, id string) (*domain.OrgInvite, error) {
	invite, ok := s.invites[id]
	if !ok {
		return nil, nil
	}
	out := *invite
	return &out, nil
}

func (s *memStore) RevokeOrgInvite(_ context.Context, inviteID string) error {
	invite, ok := s.invites[inviteID]
	switch {
	case !ok:
		return db.ErrOrgInviteNotFound
	case invite.AcceptedAt != nil:
		return db.ErrOrgInviteUsed
	case invite.RevokedAt == nil:
		now := s.now()
		invite.RevokedAt = &now
	}
	return nil
}

func (s *memStore) AcceptOrgInvite(_ context.Context, tokenHash, userID string) (*domain.OrgMember, error) {
	invite, ok := s.invites[s.inviteIDs[tokenHash]]
	switch {
	case !ok:
		return nil, db.ErrOrgInviteNotFound
	case invite.AcceptedAt != nil:
		return nil, db.ErrOrgInviteUsed
	case invite.RevokedAt != nil:
		return nil, db.ErrOrgInviteRevoked
	case !s.pending(invite):
		return nil, db.ErrOrgInviteExpired
	}
	if _, member := s.roles[invite.OrgID][userID]; member {
		return nil, db.ErrAlreadyOrgMember
	}
	now := s.now()
	invite.AcceptedBy, invite.AcceptedAt = userID, &now
	return s.join(invite.OrgID, userID, invite.Role), nil
}
