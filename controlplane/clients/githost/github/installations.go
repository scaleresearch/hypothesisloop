package github

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// The types of account GitHub installs an App on.
const (
	AccountUser         = "User"
	AccountOrganization = "Organization"
)

// Installation is one installation of the App: a GitHub account, with the repositories its
// owner chose there.
type Installation struct {
	ID           int64  `json:"installation_id"`
	AccountLogin string `json:"account_login" doc:"The GitHub user or organisation the App is installed on. It is the owner part of every repository the installation covers."`
	AccountType  string `json:"account_type" enum:"User,Organization" doc:"Whether that account is a person's own or an organisation."`
	// SelectedRepositories says the installation covers the repositories its owner selected and
	// no other.
	SelectedRepositories bool `json:"-"`
	// ChangedAt is when GitHub last changed the installation: made it, or changed its
	// repositories or permissions.
	ChangedAt time.Time `json:"-"`
}

func accountType(kind string) bool {
	return kind == AccountUser || kind == AccountOrganization
}

// Installation returns one installation of the App as GitHub states it to the App itself:
// ErrInstallationGone for one removed, ErrInstallationSuspended for one suspended, and
// PermissionMissingError for one that has not granted every permission the App asks for, as
// when its owner has yet to accept them.
func (c *Client) Installation(ctx context.Context, installationID int64) (*Installation, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	var out struct {
		ID                  int64             `json:"id"`
		SuspendedAt         *string           `json:"suspended_at"`
		UpdatedAt           time.Time         `json:"updated_at"`
		RepositorySelection string            `json:"repository_selection"`
		Permissions         map[string]string `json:"permissions"`
		Account             struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}
	path := "/app/installations/" + strconv.FormatInt(installationID, 10)
	switch err := c.call(ctx, http.MethodGet, path, jwt, nil, &out); {
	case errors.Is(err, errNotFound):
		return nil, ErrInstallationGone
	case errors.Is(err, errUnauthorized):
		return nil, ErrAppCredentials
	case err != nil:
		return nil, failedFor(installationID, err)
	}
	if out.SuspendedAt != nil {
		return nil, ErrInstallationSuspended
	}
	if out.ID != installationID || out.UpdatedAt.IsZero() || out.Account.Login == "" || !accountType(out.Account.Type) || (out.RepositorySelection != "all" && out.RepositorySelection != "selected") {
		return nil, fmt.Errorf("github: GET /app/installations/{id} answered without the installation asked for, when it last changed, its account, a user or an organisation, or whether it covers all repositories or selected ones")
	}
	if missing := missingPermissions(out.Permissions); len(missing) > 0 {
		return nil, &PermissionMissingError{Installation: installationID, Missing: missing}
	}
	return &Installation{ID: out.ID, AccountLogin: out.Account.Login, AccountType: out.Account.Type, SelectedRepositories: out.RepositorySelection == "selected", ChangedAt: out.UpdatedAt}, nil
}

const pageSize = 100

// Repositories lists every repository an installation covers, as GitHub pages them. The token
// it reads with reaches the installation's metadata and nothing else, and does not leave here.
func (c *Client) Repositories(ctx context.Context, installationID int64) ([]Repository, error) {
	token, err := c.mint(ctx, installationID, map[string]any{"permissions": map[string]string{"metadata": "read"}})
	if err != nil {
		return nil, err
	}
	found := []Repository{}
	for page := 1; ; page++ {
		var out struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				FullName string `json:"full_name"`
				Private  bool   `json:"private"`
			} `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", pageSize, page)
		if err := c.call(ctx, http.MethodGet, path, token.Token, nil, &out); err != nil {
			return nil, failedFor(installationID, err)
		}
		for _, one := range out.Repositories {
			if ValidRepository(one.FullName) != nil {
				return nil, fmt.Errorf("github: GET /installation/repositories answered a repository without its name")
			}
			found = append(found, Repository{FullName: one.FullName, Private: one.Private})
		}
		if len(out.Repositories) < pageSize || len(found) >= out.TotalCount {
			return found, nil
		}
	}
}
