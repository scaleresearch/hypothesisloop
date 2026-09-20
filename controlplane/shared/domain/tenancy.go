package domain

import (
	"fmt"
	"time"
)

// User is a person known to the identity provider. ID is the provider's subject.
type User struct {
	ID        string    `json:"id"`
	Staff     bool      `json:"staff"`
	CreatedAt time.Time `json:"created_at"`
}

type OrgRole string

const (
	OrgRoleOwner  OrgRole = "owner"
	OrgRoleMember OrgRole = "member"
)

func ValidOrgRole(r OrgRole) bool { return r == OrgRoleOwner || r == OrgRoleMember }

// Org owns projects, agents, API keys and clusters. Role is the asking user's own role and is
// set only on a per-user listing.
type Org struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      OrgRole   `json:"role,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Project groups an org's platform experiments.
type Project struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// ProjectRepo is the GitHub repository attached to a project, which its agents and jobs clone.
// RepositoryID is GitHub's own id and survives a rename. Repo is owner/name.
type ProjectRepo struct {
	ProjectID      string    `json:"project_id"`
	InstallationID int64     `json:"installation_id"`
	RepositoryID   int64     `json:"repository_id"`
	Repo           string    `json:"repo"`
	AttachedAt     time.Time `json:"attached_at"`
}

// DefaultProjectName is the project every org starts with.
const DefaultProjectName = "default"

// APIKey is a customer's credential. The token is returned once at creation and only its hash is
// stored. An empty ProjectID reaches every project of the org. A key works while it is neither revoked nor orphaned.
type APIKey struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id,omitempty"`
	CreatedBy string `json:"created_by"`
	// MintedBy is the key this one was created with, empty when a person created it.
	MintedBy  string    `json:"minted_by,omitempty"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// RevokedAt is the earliest revocation of this key or of a key it descends from.
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	// Orphaned reports that the person who created the key no longer owns its org and is not
	// staff. A key carries its creator's authority, so it ends with it.
	Orphaned bool `json:"orphaned,omitempty"`
}

// Cluster is a runtime's registered name and the org that added it. Shared is the platform's
// decision that the cluster takes every org's jobs. ReportedID is the identity its runtime
// states about itself, empty until its first report.
type Cluster struct {
	Name       string    `json:"name"`
	OrgID      string    `json:"org_id"`
	Shared     bool      `json:"shared"`
	ReportedID string    `json:"cluster_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// MaxClusterIDLength bounds the identity a runtime states: a kube-system namespace UID or a
// machine id, with room to spare.
const MaxClusterIDLength = 128

// ValidateClusterID refuses an identity that could not be one. It is stored, compared and used
// as a path segment, so its characters are held to the ones those identities are made of.
func ValidateClusterID(id string) error {
	if id == "" || len(id) > MaxClusterIDLength {
		return fmt.Errorf("cluster_id must be 1 to %d characters", MaxClusterIDLength)
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return fmt.Errorf("cluster_id may hold only letters, digits, '-', '_' and '.'")
		}
	}
	return nil
}

// Hosts reports whether a job of orgID may be placed on the cluster: one of its own org, or one
// the platform shares.
func (c Cluster) Hosts(orgID string) bool {
	return c.Shared || c.OrgID == orgID
}

// AgentWindDown is how long an agent outlives its platform experiment closing, or its own cut:
// its token keeps working and its machine keeps running, so its last turn can file summaries.
const AgentWindDown = 120 * time.Second

// Tenancy places one platform experiment: org is always derived through its project. AgentID is
// set when what was placed is a job or a donation request, which belong to one agent.
type Tenancy struct {
	PlatformExperimentID string
	ProjectID            string
	OrgID                string
	AgentID              string
}

// TenantScope is what one caller may see. AllOrgs lifts the org restriction; ProjectID and
// PlatformExperimentID each narrow further when set.
type TenantScope struct {
	AllOrgs              bool
	OrgIDs               []string
	ProjectID            string
	PlatformExperimentID string
}

// AllTenants is the scope of the platform's own loops and of staff.
var AllTenants = TenantScope{AllOrgs: true}

// Unrestricted reports whether the scope admits every row.
func (s TenantScope) Unrestricted() bool {
	return s.AllOrgs && s.ProjectID == "" && s.PlatformExperimentID == ""
}

// Admits reports whether a platform experiment placed at t is inside the scope.
func (s TenantScope) Admits(t Tenancy) bool {
	if !s.AllOrgs && !contains(s.OrgIDs, t.OrgID) {
		return false
	}
	if s.ProjectID != "" && s.ProjectID != t.ProjectID {
		return false
	}
	return s.PlatformExperimentID == "" || s.PlatformExperimentID == t.PlatformExperimentID
}

// AdmitsOrg reports whether an org-level resource (an agent, a cluster) is inside the scope.
func (s TenantScope) AdmitsOrg(orgID string) bool {
	return s.AllOrgs || contains(s.OrgIDs, orgID)
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

type HumanWriteKind string

const (
	HumanWriteHypothesis HumanWriteKind = "hypothesis"
	HumanWriteLink       HumanWriteKind = "link"
	HumanWriteComment    HumanWriteKind = "comment"
)

// HumanWrite records which signed-in user made a contribution to the hypothesis graph. The
// contribution itself carries Author, a name the user chooses with the identity provider, so
// the account is kept beside it. Subject is the hypothesis id, the comment id, or for a link
// "<from> <type> <to>".
type HumanWrite struct {
	UserID               string
	Author               string
	Kind                 HumanWriteKind
	Subject              string
	PlatformExperimentID string
}
