package domain

import (
	"fmt"
	"slices"
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

// OrgMember is one person of an org. The platform holds no name and no email for them: the
// identity provider does.
type OrgMember struct {
	OrgID    string    `json:"org_id"`
	UserID   string    `json:"user_id"`
	Role     OrgRole   `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// InviteLifetime is how long an invite to an org can be accepted.
const InviteLifetime = 7 * 24 * time.Hour

// OrgInvite is a link that lets one signed-in person join an org with Role. Its token is returned
// once at creation and only its hash is stored. It works once, until ExpiresAt, and while not
// revoked. CreatedBy is the person answerable for it.
type OrgInvite struct {
	ID         string     `json:"id"`
	OrgID      string     `json:"org_id"`
	Role       OrgRole    `json:"role"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	AcceptedBy string     `json:"accepted_by,omitempty"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Project groups an org's platform experiments.
type Project struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// OrgGitHubInstallation is an installation of the platform's GitHub App that an org connected.
// AccountLogin is the GitHub account it is on, the owner part of every repository it covers.
type OrgGitHubInstallation struct {
	OrgID          string    `json:"org_id"`
	InstallationID int64     `json:"installation_id"`
	AccountLogin   string    `json:"account_login"`
	ConnectedBy    string    `json:"connected_by" doc:"The user who connected it."`
	ConnectedAt    time.Time `json:"connected_at"`
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
	// Orphaned reports that the person who created the key could no longer create it: they left
	// the org, or the key reaches the whole org and they no longer own it, and they are not staff.
	// A key carries its creator's authority, so it ends with it.
	Orphaned bool `json:"orphaned,omitempty"`
}

// Cluster is a runtime's registered name and the org that added it. Shared is the platform's
// decision that the cluster takes every org's jobs. ReportedID is the identity its runtime
// states about itself, empty until its first report. DisabledOrgs are the orgs that turned the
// cluster off for their own jobs.
type Cluster struct {
	Name         string    `json:"name"`
	OrgID        string    `json:"org_id"`
	Shared       bool      `json:"shared"`
	ReportedID   string    `json:"cluster_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	DisabledOrgs []string  `json:"-"`
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

// Hosts reports whether a job of orgID may be placed on the cluster: one open to the org that the
// org has not turned off.
func (c Cluster) Hosts(orgID string) bool {
	return c.OpenTo(orgID) && c.EnabledFor(orgID)
}

// OpenTo reports whether the cluster is one orgID can use: one of its own, or one the platform
// shares.
func (c Cluster) OpenTo(orgID string) bool {
	return c.Shared || c.OrgID == orgID
}

// EnabledFor reports whether orgID left the cluster on for its jobs, which is the default.
func (c Cluster) EnabledFor(orgID string) bool {
	return !slices.Contains(c.DisabledOrgs, orgID)
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
	// Shared is the platform's decision that every account may read the platform experiment.
	Shared bool
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
