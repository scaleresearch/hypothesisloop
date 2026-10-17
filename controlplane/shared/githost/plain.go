package githost

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// Plain is any git host that serves repositories under one root over smart HTTP. A repository is
// a name under that root, made on the host by whoever runs it: the platform creates none and
// reads no file from one. Its one credential is the deployment's, the same for every repository
// and every caller.
type Plain struct {
	base     *url.URL
	username string
	password string
	client   *http.Client
}

var _ Provider = (*Plain)(nil)

var plainRepository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// NewPlain takes the root every repository is under and the credential the host takes, both
// empty for a host that asks for none.
func NewPlain(baseURL, username, password string) (*Plain, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("git base_url %q must be the root repositories are cloned from, as scheme://host[:port][/path] with no credential in it", baseURL)
	}
	if (username == "") != (password == "") {
		return nil, fmt.Errorf("the git host's username and password are given together or not at all")
	}
	return &Plain{base: base, username: username, password: password, client: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (p *Plain) URL(repository string) string { return p.base.String() + "/" + repository }

// Reachable asks the host for the repository's refs, the request git ls-remote makes.
func (p *Plain) Reachable(ctx context.Context, _, repository string) (string, error) {
	if !plainRepository.MatchString(repository) || len(repository) > 200 {
		return "", refusal.Refuse(http.StatusUnprocessableEntity, "invalid_repository", "repository must be the name of a repository under "+p.base.String()+": path segments of letters, digits, '.', '_' and '-'")
	}
	address := p.URL(repository)
	notReachable := func(why string) error {
		return refusal.Refuse(http.StatusUnprocessableEntity, "repository_not_reachable", "no repository answers git at "+address+" ("+why+"): it is created on the git host before a platform experiment names it")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return "", notReachable("the address is not one")
	}
	if p.username != "" {
		req.SetBasicAuth(p.username, p.password)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", notReachable("the host did not answer")
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", notReachable(fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	if resp.Header.Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
		return "", notReachable("the answer is not git's")
	}
	return repository, nil
}

func (p *Plain) CreatesOn(context.Context, string, string) (string, error) {
	return "", p.createsNone()
}

func (p *Plain) Create(context.Context, string, string, string, string, map[string]string) (string, error) {
	return "", p.createsNone()
}

func (p *Plain) createsNone() error {
	return refusal.Refuse(http.StatusUnprocessableEntity, "repository_required", "the platform creates no repository on this deployment's git host: create one under "+p.base.String()+" and name it in repository")
}

func (p *Plain) File(_ context.Context, _, _, path string) ([]byte, error) {
	return nil, refusal.Refuse(http.StatusUnprocessableEntity, "experiment_file_unsupported", "this deployment's git host hands the platform no file, so "+path+" cannot be read from the repository: state its fields in the request")
}

// FileAt finds no file: the platform reads none from a plain host, so to it every repository
// there declares nothing, an environment included.
func (p *Plain) FileAt(context.Context, string, string, string, string) ([]byte, error) {
	return nil, ErrFileNotFound
}

// ErrNoCredential is Credential's answer on a host that takes none.
var ErrNoCredential = refusal.Refuse(http.StatusNotFound, "not_found", "this deployment's git host takes no credential")

func (p *Plain) Credential(context.Context, string, string, bool) (*Credential, error) {
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
