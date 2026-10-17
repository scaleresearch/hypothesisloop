package githost

import (
	"context"
	"errors"
	"time"
)

// The reasons of a refusal any git host gives a client, which tells them apart whatever the
// provider.
const (
	ReasonNotConnected      = "git_host_not_connected"
	ReasonPermissionMissing = "git_host_permission_missing"
	ReasonFailed            = "git_host_failed"
)

// ErrFileNotFound is FileAt's answer for a path the repository does not hold at that ref.
var ErrFileNotFound = errors.New("githost: no such file")

// Provider is the git host the platform experiments of a deployment keep their code on. A
// repository is the name the host knows it by. What a call refuses is returned as the API
// answers it.
type Provider interface {
	// URL is the address a repository of that name is cloned from.
	URL(repository string) string
	// Reachable returns the repository as the host names it, when the project may keep a
	// platform experiment's code in it.
	Reachable(ctx context.Context, projectID, repository string) (string, error)
	// CreatesOn returns the account Create makes a repository the project names none of on,
	// account or the project's only or default one, or what refuses a repository there: everything
	// Create checks before it creates, asked of the host now, so a request is answered with it
	// before anything is made. A host with no accounts refuses: there a repository is named.
	CreatesOn(ctx context.Context, projectID, account string) (string, error)
	// Create makes the repository from the deployment's experiment template and returns it, as
	// Reachable names it, and its default branch, which holds one commit of the template's files.
	// It refuses a repository that exists.
	Create(ctx context.Context, projectID, repository, description string) (created, branch string, err error)
	// HasBranch says whether the repository has a branch of that name.
	HasBranch(ctx context.Context, projectID, repository, branch string) (bool, error)
	// File reads a file of the repository at ref for a form to show: ErrFileNotFound when it
	// holds none there. A host that serves no file to the platform refuses.
	File(ctx context.Context, projectID, repository, ref, path string) ([]byte, error)
	// FileAt reads a file of the repository at ref, a branch or a commit: ErrFileNotFound when
	// it holds none there.
	FileAt(ctx context.Context, projectID, repository, ref, path string) ([]byte, error)
	// Credential mints what git authenticates to that one repository with. write says whether
	// it may push.
	Credential(ctx context.Context, projectID, repository string, write bool) (*Credential, error)
	// Serves reports whether an address git asks a credential for is the repository's.
	Serves(repository, protocol, host, path string) bool
}

// Credential is what git sends a host. ExpiresAt is zero for one that does not run out.
type Credential struct {
	Username  string
	Password  string
	ExpiresAt time.Time
}
