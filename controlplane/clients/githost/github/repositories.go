package github

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Repository is a repository an installation covers, named owner/name in GitHub's own casing.
type Repository struct {
	FullName string
	Private  bool
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

// CreateRepository creates a private repository of that name on the account an installation is
// on, whose default branch holds one commit: files, by path. The installation creates it with a
// token that holds everything it granted: on an organisation through the organisation's
// repositories, on a person's own account through "the authenticated user's", which GitHub
// documents as taking an installation token with Administration (write). The token does not
// leave here.
func (c *Client) CreateRepository(ctx context.Context, installationID int64, name, description string, files map[string]string) (*Repository, error) {
	if !newRepositoryName.MatchString(name) || name == "." || name == ".." {
		return nil, ErrInvalidRepository
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("github: a repository is created with the files of its first commit")
	}
	installation, err := c.Installation(ctx, installationID)
	if err != nil {
		return nil, err
	}
	owner := installation.AccountLogin
	path := "/orgs/" + url.PathEscape(owner) + "/repos"
	if installation.AccountType == AccountUser {
		path = "/user/repos"
	}
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
	// GitHub writes no tree to a repository with no commit, so it makes the first one itself
	// and firstCommit takes its place.
	body := map[string]any{"name": name, "description": description, "private": true, "auto_init": true}
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
	if out.ID <= 0 || ValidRepository(out.FullName) != nil {
		return nil, fmt.Errorf("github: POST %s answered without the repository it created", path)
	}
	if !out.Private || !strings.EqualFold(out.FullName, owner+"/"+name) {
		return nil, &NotAsAskedError{Repository: out.FullName}
	}
	write, err := c.Token(ctx, installationID, out.FullName, Write)
	if errors.Is(err, ErrRepositoryNotInstalled) {
		return nil, &NotCoveredError{Repository: out.FullName, Cause: err}
	}
	if err != nil {
		return nil, &FirstCommitError{Repository: out.FullName, Cause: err}
	}
	if err := c.firstCommit(ctx, write, out.DefaultBranch, files); err != nil {
		return nil, &FirstCommitError{Repository: out.FullName, Cause: err}
	}
	return &Repository{FullName: out.FullName, Private: true}, nil
}

// StartFresh makes files the only commit of the token's repository, a write token's, when it
// holds nothing but the commit GitHub made when it was created: what a CreateRepository cut off
// before its first commit leaves. ErrNotFresh for one that holds anything more.
func (c *Client) StartFresh(ctx context.Context, token *Token, files map[string]string) error {
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.call(ctx, http.MethodGet, "/repos/"+token.Repository, token.Value, nil, &repository); err != nil {
		return failedFor(token.installation, err)
	}
	var commits []struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	path := "/repos/" + token.Repository + "/commits?per_page=2&sha=" + url.QueryEscape(repository.DefaultBranch)
	if err := c.call(ctx, http.MethodGet, path, token.Value, nil, &commits); err != nil {
		return failedFor(token.installation, err)
	}
	if len(commits) != 1 || len(commits[0].Parents) != 0 {
		return ErrNotFresh
	}
	return failedFor(token.installation, c.firstCommit(ctx, token, repository.DefaultBranch, files))
}

// firstCommit makes files the only commit of a branch the token's repository has: a tree, a
// commit of it with no parent, and the branch moved there whatever it held.
func (c *Client) firstCommit(ctx context.Context, token *Token, branch string, files map[string]string) error {
	if branch == "" {
		return fmt.Errorf("github: %s names no default branch", token.Repository)
	}
	entries := make([]map[string]string, 0, len(files))
	for _, path := range slices.Sorted(maps.Keys(files)) {
		entries = append(entries, map[string]string{"path": path, "mode": "100644", "type": "blob", "content": files[path]})
	}
	git := "/repos/" + token.Repository + "/git/"
	var tree, commit struct {
		SHA string `json:"sha"`
	}
	if err := c.call(ctx, http.MethodPost, git+"trees", token.Value, map[string]any{"tree": entries}, &tree); err != nil {
		return err
	}
	made := map[string]any{"message": "Start from the HypothesisLoop experiment template", "tree": tree.SHA, "parents": []string{}}
	if err := c.call(ctx, http.MethodPost, git+"commits", token.Value, made, &commit); err != nil {
		return err
	}
	if tree.SHA == "" || commit.SHA == "" {
		return fmt.Errorf("github: %s answered a tree or a commit without its sha", token.Repository)
	}
	return c.call(ctx, http.MethodPatch, git+"refs/heads/"+branch, token.Value, map[string]any{"sha": commit.SHA, "force": true}, nil)
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
