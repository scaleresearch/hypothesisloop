package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Repository is a repository an installation covers, named owner/name in GitHub's own casing.
// DefaultBranch is said of one CreateFromTemplate made.
type Repository struct {
	FullName      string
	Private       bool
	DefaultBranch string
}

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+$`)

// ValidRepository refuses a name that is not owner/name. The name becomes part of a URL, so this
// is also what keeps a caller from steering a request elsewhere.
func ValidRepository(fullName string) error {
	_, name, _ := strings.Cut(fullName, "/")
	if !repositoryName.MatchString(fullName) || name == "." || name == ".." || len(fullName) > 200 {
		return ErrInvalidRepository
	}
	return nil
}

// gitHost is the host git asks credentials for when it clones a repository.
const gitHost = "github.com"

// CloneURL is the HTTPS address git clones the repository from.
func CloneURL(fullName string) string {
	return webURL + "/" + fullName + ".git"
}

var newRepositoryName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// CreateFromTemplate creates a private repository of that name on the account an installation is
// on, generated from template, a public template repository named owner/name: its default branch
// holds one commit of the template's files. It returns once that branch exists. The installation
// creates it with a token that holds everything it granted, which does not leave here.
func (c *Client) CreateFromTemplate(ctx context.Context, installationID int64, template, name, description string) (*Repository, error) {
	if !newRepositoryName.MatchString(name) || name == "." || name == ".." {
		return nil, ErrInvalidRepository
	}
	if err := ValidRepository(template); err != nil {
		return nil, fmt.Errorf("github: the experiment template %q is not owner/name", template)
	}
	installation, err := c.Installation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	owner := installation.AccountLogin
	// GitHub documents no name by which a token is narrowed to creating repositories, so the
	// token is asked for whole.
	token, err := c.mint(ctx, installationID, map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
	}
	path := "/repos/" + template + "/generate"
	body := map[string]any{"owner": owner, "name": name, "description": description, "private": true}
	var refused *rejection
	switch err := c.call(ctx, http.MethodPost, path, token.Token, body, &out); {
	case errors.Is(err, ErrRateLimited):
		return nil, err
	case errors.Is(err, errUnprocessable) && errors.As(err, &refused) && refused.nameExists:
		return nil, ErrRepositoryExists
	case errors.Is(err, errForbidden) && errors.As(err, &refused), errors.Is(err, errNotFound) && errors.As(err, &refused):
		return nil, &CreationNotPermittedError{Accepted: refused.accepted}
	case errors.As(err, &refused):
		return nil, &CreationFailedError{Status: refused.status, Message: refused.message}
	case err != nil:
		return nil, failedFor(installationID, err)
	}
	if out.ID <= 0 || ValidRepository(out.FullName) != nil || out.DefaultBranch == "" {
		return nil, fmt.Errorf("github: POST %s answered without the repository it created", routeOf(path))
	}
	if !out.Private || !strings.EqualFold(out.FullName, owner+"/"+name) {
		return nil, &NotAsAskedError{Repository: out.FullName}
	}
	read, err := c.Token(ctx, installationID, out.FullName, Read)
	if errors.Is(err, ErrRepositoryNotInstalled) {
		return nil, &NotCoveredError{Repository: out.FullName, Cause: err}
	}
	if err != nil {
		return nil, err
	}
	// GitHub copies the template's files after it answers, and the branch exists once it has.
	for wait := time.Duration(0); ; wait = time.Second {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		exists, err := c.HasBranch(ctx, read, out.DefaultBranch)
		if err != nil {
			return nil, err
		}
		if exists {
			return &Repository{FullName: out.FullName, Private: true, DefaultBranch: out.DefaultBranch}, nil
		}
	}
}

// HasBranch says whether the token's repository has a branch of that name.
func (c *Client) HasBranch(ctx context.Context, token *Token, branch string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "/repos/"+token.Repository+"/branches/"+escapedPath(branch), token.Value, nil, nil)
	switch {
	case errors.Is(err, errNotFound):
		return false, nil
	case err != nil:
		return false, failedFor(token.installation, err)
	}
	return true, nil
}

// escapedPath is a path of segments, each escaped for a URL.
func escapedPath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// File reads one file of a repository with a token minted for it, at ref, a branch name or a
// commit. An empty ref is the head of the repository's default branch as it stands now.
func (c *Client) File(ctx context.Context, token *Token, ref, path string) ([]byte, error) {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return nil, fmt.Errorf("github: %q is not a path inside a repository", path)
		}
		segments[i] = url.PathEscape(segment)
	}
	target := "/repos/" + token.Repository + "/contents/" + strings.Join(segments, "/")
	if ref != "" {
		target += "?ref=" + url.QueryEscape(ref)
	}
	var raw json.RawMessage
	switch err := c.request(ctx, http.MethodGet, target, "Bearer "+token.Value, maxFileResponseBytes, nil, &raw); {
	case errors.Is(err, errNotFound):
		return nil, ErrFileNotFound
	case err != nil:
		return nil, failedFor(token.installation, err)
	}
	var file struct {
		Type     string `json:"type"`
		Size     int64  `json:"size"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	// A directory is answered as a list of its entries, a symlink and a submodule under their
	// own type.
	if json.Unmarshal(raw, &file) != nil || file.Type != "file" {
		return nil, ErrNotAFile
	}
	if file.Size > MaxFileBytes {
		return nil, ErrFileTooLarge
	}
	if file.Encoding != "base64" {
		return nil, fmt.Errorf("github: the contents of %s came back in encoding %q", path, file.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github: the contents of %s are not base64", path)
	}
	return data, nil
}
