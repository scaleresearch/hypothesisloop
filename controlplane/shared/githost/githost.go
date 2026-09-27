package githost

import (
	"context"
	"time"
)

// Provider is the git host the platform experiments of a deployment keep their code on. A
// repository is the name the host knows it by. What a call refuses is returned as the API
// answers it.
type Provider interface {
	// URL is the address a repository of that name is cloned from.
	URL(repository string) string
	// Reachable returns the repository as the host names it, when the project may keep a
	// platform experiment's code in it.
	Reachable(ctx context.Context, projectID, repository string) (string, error)
	// Create makes a repository of that name for a platform experiment that names none, on the
	// account named where the host has several for the project, and returns it. files, by path,
	// are its first commit. A host that creates none refuses. A repository the request names is
	// never written to.
	Create(ctx context.Context, projectID, account, name, description string, files map[string]string) (string, error)
	// File reads a file at the head of the repository's default branch. A host that serves no
	// file to the platform refuses.
	File(ctx context.Context, projectID, repository, path string) ([]byte, error)
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
