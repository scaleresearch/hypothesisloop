package githost

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// Projects is what Plain reads of the project a repository is asked for. *db.Store satisfies it.
type Projects interface {
	GetProject(ctx context.Context, id string) (*domain.Project, error)
}

// Plain is any git host that serves repositories under one root over smart HTTP. A repository is
// a name under that root. The platform creates one by pushing the experiment template's files to
// it, which a host that creates a repository on its first push takes, and reads no file from one.
// Its one credential is the deployment's, the same for every repository, and never leaves the
// control plane. The host knows no orgs, so an org's repositories are under its id: a name a
// request gives is read there, and every access is refused a repository outside the org of the
// project it is made for.
type Plain struct {
	base     *url.URL
	username string
	password string
	template string
	projects Projects
	client   *http.Client
}

var _ Provider = (*Plain)(nil)

var plainRepository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// NewPlain takes the root every repository is under, the credential the host takes, both empty
// for a host that asks for none, the experiment template, a repository under the root, and the
// projects each access is checked against.
func NewPlain(baseURL, username, password, template string, projects Projects) (*Plain, error) {
	if projects == nil {
		return nil, errors.New("the git host needs the projects to bind repositories to their org")
	}
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("git base_url %q must be the root repositories are cloned from, as scheme://host[:port][/path] with no credential in it", baseURL)
	}
	if (username == "") != (password == "") {
		return nil, fmt.Errorf("the git host's username and password are given together or not at all")
	}
	if !plainRepository.MatchString(template) {
		return nil, fmt.Errorf("the experiment template %q must be the name of a repository under %s", template, baseURL)
	}
	return &Plain{base: base, username: username, password: password, template: template, projects: projects, client: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (p *Plain) URL(repository string) string { return p.base.String() + "/" + repository }

// orgOf is the org of the project, the directory its repositories are under.
func (p *Plain) orgOf(ctx context.Context, projectID string) (string, error) {
	project, err := p.projects.GetProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	if project == nil {
		return "", refusal.NotFound("project")
	}
	if !plainRepository.MatchString(project.OrgID) || strings.Contains(project.OrgID, "/") {
		return "", fmt.Errorf("githost: org id %q is not a directory name", project.OrgID)
	}
	return project.OrgID, nil
}

// named is the repository a request names for the project, under the directory of its org: a name
// already under it is taken as it is, as the host names it.
func (p *Plain) named(ctx context.Context, projectID, repository string) (string, error) {
	if err := p.validName(repository); err != nil {
		return "", err
	}
	org, err := p.orgOf(ctx, projectID)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(repository, org+"/") {
		repository = org + "/" + repository
	}
	if err := p.validName(repository); err != nil {
		return "", err
	}
	return repository, nil
}

// owned refuses a repository, as the host names it, outside the directory of the project's org.
func (p *Plain) owned(ctx context.Context, projectID, repository string) error {
	if err := p.validName(repository); err != nil {
		return err
	}
	org, err := p.orgOf(ctx, projectID)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(repository, org+"/") {
		return refusal.Refuse(http.StatusUnprocessableEntity, "repository_not_reachable", repository+" is not a repository of this org: on this deployment's git host an org's repositories are under "+org+"/")
	}
	return nil
}

// Reachable asks the host for the refs of the repository named under the org's directory, the
// request git ls-remote makes.
func (p *Plain) Reachable(ctx context.Context, projectID, repository string) (string, error) {
	named, err := p.named(ctx, projectID, repository)
	if err != nil {
		return "", err
	}
	if err := p.reachable(ctx, named); err != nil {
		return "", err
	}
	return named, nil
}

func (p *Plain) reachable(ctx context.Context, repository string) error {
	address := p.URL(repository)
	notReachable := func(why string) error {
		return refusal.Refuse(http.StatusUnprocessableEntity, "repository_not_reachable", "no repository answers git at "+address+" ("+why+")")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return notReachable("the address is not one")
	}
	if p.username != "" {
		req.SetBasicAuth(p.username, p.password)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return notReachable("the host did not answer")
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return notReachable(fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	if resp.Header.Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
		return notReachable("the answer is not git's")
	}
	return nil
}

func (p *Plain) validName(repository string) error {
	if !plainRepository.MatchString(repository) || len(repository) > 200 {
		return refusal.Refuse(http.StatusUnprocessableEntity, "invalid_repository", "repository must be the name of a repository under "+p.base.String()+": path segments of letters, digits, '.', '_' and '-'")
	}
	return nil
}

// CreatesOn refuses: a plain host has no accounts, so a repository it creates is named.
func (p *Plain) CreatesOn(context.Context, string, string) (string, error) {
	return "", refusal.Refuse(http.StatusUnprocessableEntity, "repository_required", "on this deployment's git host a platform experiment names its repository, a name under "+p.base.String()+", which the platform creates from the experiment template when it does not exist")
}

// Create pushes one commit of the template's files, with no history, to the repository as the
// branch the template's HEAD names.
func (p *Plain) Create(ctx context.Context, projectID, repository, _ string) (string, string, error) {
	repository, err := p.named(ctx, projectID, repository)
	if err != nil {
		return "", "", err
	}
	if p.reachable(ctx, repository) == nil {
		return "", "", refusal.Refuse(http.StatusConflict, "repository_exists", repository+" already exists under "+p.base.String())
	}
	dir, err := os.MkdirTemp("", "hl-create-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir) //nolint:errcheck
	template := p.URL(p.template)
	head, err := p.git(ctx, dir, "ls-remote", "--symref", template, "HEAD")
	if err != nil {
		return "", "", notCreated(repository, err)
	}
	ref, _, _ := strings.Cut(strings.TrimPrefix(strings.SplitN(head, "\n", 2)[0], "ref: "), "\t")
	if !strings.HasPrefix(ref, "refs/heads/") {
		return "", "", notCreated(repository, fmt.Errorf("the experiment template %s names no branch as its HEAD", template))
	}
	var commit string
	for _, args := range [][]string{{"init", "--quiet", "--bare"}, {"fetch", "--quiet", "--depth=1", template, ref}} {
		if _, err := p.git(ctx, dir, args...); err != nil {
			return "", "", notCreated(repository, err)
		}
	}
	if commit, err = p.git(ctx, dir, "commit-tree", "FETCH_HEAD^{tree}", "-m", "Start from the experiment template"); err != nil {
		return "", "", notCreated(repository, err)
	}
	if _, err := p.git(ctx, dir, "push", "--quiet", p.URL(repository), commit+":"+ref); err != nil {
		return "", "", notCreated(repository, err)
	}
	return repository, strings.TrimPrefix(ref, "refs/heads/"), nil
}

func notCreated(repository string, err error) error {
	return refusal.Refuse(http.StatusBadGateway, "repository_not_created", "the git host did not take "+repository+" from the experiment template: "+err.Error())
}

func (p *Plain) HasBranch(ctx context.Context, projectID, repository, branch string) (bool, error) {
	if err := p.owned(ctx, projectID, repository); err != nil {
		return false, err
	}
	heads, err := p.git(ctx, "", "ls-remote", "--heads", p.URL(repository), "refs/heads/"+branch)
	if err != nil {
		return false, refusal.Refuse(http.StatusBadGateway, "repository_not_reachable", "the branches of "+repository+" could not be read: "+err.Error())
	}
	return heads != "", nil
}

// git runs git in dir with the host's credential as a header, never in a URL or an argument.
func (p *Plain) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=HypothesisLoop", "GIT_AUTHOR_EMAIL=platform@hypothesisloop.invalid",
		"GIT_COMMITTER_NAME=HypothesisLoop", "GIT_COMMITTER_EMAIL=platform@hypothesisloop.invalid")
	if p.username != "" {
		basic := base64.StdEncoding.EncodeToString([]byte(p.username + ":" + p.password))
		cmd.Env = append(cmd.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
	}
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(exit.Stderr)))
	}
	return strings.TrimSpace(string(out)), err
}

func (p *Plain) File(_ context.Context, _, _, _, path string) ([]byte, error) {
	return nil, refusal.Refuse(http.StatusUnprocessableEntity, "experiment_file_unsupported", "this deployment's git host hands the platform no file, so "+path+" cannot be read from the repository: state its fields in the request")
}

// FileAt finds no file in a repository of the org: the platform reads none from a plain host, so
// to it every repository there declares nothing, an environment included.
func (p *Plain) FileAt(ctx context.Context, projectID, repository, _, _ string) ([]byte, error) {
	if err := p.owned(ctx, projectID, repository); err != nil {
		return nil, err
	}
	return nil, ErrFileNotFound
}

// ErrNoCredential is Credential's answer on a host that takes none.
var ErrNoCredential = refusal.Refuse(http.StatusNotFound, "not_found", "this deployment's git host takes no credential")

func (p *Plain) Credential(ctx context.Context, projectID, repository string, _ bool) (*Credential, error) {
	if err := p.owned(ctx, projectID, repository); err != nil {
		return nil, err
	}
	if p.username == "" {
		return nil, ErrNoCredential
	}
	return &Credential{Username: p.username, Password: p.password}, nil
}

// Serves takes the repository's address with or without .git, or its LFS endpoint under it.
func (p *Plain) Serves(repository, protocol, host, path string) bool {
	asked := strings.TrimSuffix(strings.Trim(path, "/"), "/info/lfs")
	served := strings.Trim(p.base.Path+"/"+repository, "/")
	return protocol == p.base.Scheme && strings.EqualFold(host, p.base.Host) && strings.TrimSuffix(asked, ".git") == strings.TrimSuffix(served, ".git")
}
