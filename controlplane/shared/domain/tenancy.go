package domain

import (
	"fmt"
	"slices"
	"time"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
)

// User is a person known to the identity provider. ID is the provider's subject.
// User is a person. Name is the display name their identity provider last stated, empty when it
// has stated none.
type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
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

// OrgMember is one person of an org. Name is the person's display name, empty when their
// identity provider has stated none.
type OrgMember struct {
	OrgID    string    `json:"org_id"`
	UserID   string    `json:"user_id"`
	Name     string    `json:"name"`
	Role     OrgRole   `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// InviteLifetime is how long an invite to an org can be accepted.
const InviteLifetime = 7 * 24 * time.Hour

// OrgInvite is a link that lets one signed-in person join an org with Role. Its token is returned
// once at creation and only its hash is stored. It works once, until ExpiresAt, and while not
// revoked. Actor is the person answerable for it: who made it, or for one made with an API key
// the person that key answers to. AcceptedActor is the person who joined with it.
type OrgInvite struct {
	ID            string     `json:"id"`
	OrgID         string     `json:"org_id"`
	Role          OrgRole    `json:"role"`
	Actor         Actor      `json:"actor"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	AcceptedActor *Actor     `json:"accepted_actor,omitempty"`
	AcceptedAt    *time.Time `json:"accepted_at,omitempty"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
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
// AccountType is empty on one connected before the type was kept, until it is connected again.
type OrgGitHubInstallation struct {
	OrgID          string    `json:"org_id"`
	InstallationID int64     `json:"installation_id"`
	AccountLogin   string    `json:"account_login"`
	AccountType    string    `json:"account_type,omitempty" enum:"User,Organization" doc:"Whether the account is a person's own or an organisation, as GitHub says. On a person's own account the platform creates a repository with the authorisation its owner connected it with. Absent on an installation connected before the type was kept, until it is connected again."`
	ConnectedActor Actor     `json:"connected_actor" doc:"Who connected it: a user, the API key it was connected with, or staff."`
	ConnectedAt    time.Time `json:"connected_at"`
	Default        bool      `json:"default" doc:"A new repository is created on this account when a request names no repository_account. At most one of an org's installations is the default. With one installation connected, that one is used whether or not it is the default."`
}

// DefaultOrgName is the org a person starts with at their first sign-in, and DefaultProjectName
// the project every org starts with. Neither is drawn from who the person is. An org is told from
// another by its id, so many carry this name.
const (
	DefaultOrgName     = "Default org"
	DefaultProjectName = "Default project"
)

// APIKey is a customer's credential. The token is returned once at creation and only its hash is
// stored. An empty ProjectID reaches every project of the org. A key works while it is neither revoked nor orphaned.
type APIKey struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id,omitempty"`
	// Actor is the person the key answers to: who created it, or for a key created with a key
	// the person that one answers to. The key carries their authority.
	Actor Actor `json:"actor"`
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
// cluster off for their own jobs. Priority orders the clusters a job may be placed on, higher
// first: its org sets ClusterPriorityMin to ClusterPriorityMax, and a shared cluster holds
// SharedClusterPriority, below every org's own.
type Cluster struct {
	Name       string `json:"name"`
	OrgID      string `json:"org_id"`
	Shared     bool   `json:"shared"`
	Priority   int    `json:"priority"`
	ReportedID string `json:"cluster_id,omitempty"`
	// JobImage is the one image the cluster runs, as its runtime reports it. Empty for a cluster
	// that runs whatever image a job names.
	JobImage string `json:"job_image,omitempty"`
	ClusterSettings
	CreatedAt    time.Time `json:"created_at"`
	DisabledOrgs []string  `json:"-"`
}

// The priorities a cluster holds. DefaultClusterPriority is the one a cluster is registered with.
const (
	SharedClusterPriority  = 0
	ClusterPriorityMin     = 1
	ClusterPriorityMax     = 1000
	DefaultClusterPriority = 100
)

// ValidateClusterOrder checks that order names each of the ranked clusters exactly once and
// nothing else.
func ValidateClusterOrder(ranked, order []string) error {
	if len(order) > ClusterPriorityMax {
		return fmt.Errorf("an order holds at most %d clusters", ClusterPriorityMax)
	}
	left := make(map[string]bool, len(ranked))
	for _, name := range ranked {
		left[name] = true
	}
	for _, name := range order {
		if !left[name] {
			return fmt.Errorf("%s is named twice or is not a cluster the org ranks", name)
		}
		delete(left, name)
	}
	for name := range left {
		return fmt.Errorf("%s is missing", name)
	}
	return nil
}

// MaxJobImageLength bounds the image a runtime reports for its cluster.
const MaxJobImageLength = 512

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

// HostsJob is the placement rule: the cluster takes jobs of orgID and runs a job that names
// image.
func (c Cluster) HostsJob(orgID, image string) bool {
	return c.Hosts(orgID) && c.Runs(image)
}

// Runs reports whether a job naming image can run on the cluster. A cluster with one image runs
// a job that names it, and any other cluster runs the image a job names. Every job names one
// (submit refuses a job without), so an image that is empty runs nowhere.
func (c Cluster) Runs(image string) bool {
	if c.JobImage == "" {
		return image != ""
	}
	return image == c.JobImage
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

// CoordinatorWindDown is AgentWindDown for a coordinator: its last turn after the close writes
// the run's final summary, which takes reading the standings and the graph first.
const CoordinatorWindDown = 5 * time.Minute

// Tenancy places one platform experiment: org is always derived through its project. AgentID is
// set when what was placed is a job, which belongs to one agent.
type Tenancy struct {
	PlatformExperimentID string
	ProjectID            string
	OrgID                string
	AgentID              string
	// Shared is the platform's decision that every account may read the platform experiment.
	Shared bool
	// PriorRuns are every other closed platform experiment of the project that works on the same
	// repository and ran (its researchers started or it declared a baseline), the runs its
	// continues chain carries on among them. Its agents and its
	// coordinator read them as their own. Set when what was placed is a platform experiment.
	PriorRuns []string
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

// ReasonClusterNotRegistered is the reason on the 401 a runtime token gets once its cluster is
// no longer registered. A runtime reads it as its cluster having been removed: it stops the
// workloads it holds and exits. Any other 401 is a token somebody has to fix, and the runtime
// keeps what it runs.
const ReasonClusterNotRegistered = clusterapi.ReasonClusterNotRegistered
