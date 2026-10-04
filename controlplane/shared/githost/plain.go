package githost

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
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
		return "", api.Refuse(http.StatusUnprocessableEntity, "invalid_repository", "repository must be the name of a repository under "+p.base.String()+": path segments of letters, digits, '.', '_' and '-'")
	}
	if _, err := p.refs(ctx, repository); err != nil {
		return "", err
	}
	return repository, nil
}

// Head reads the commit HEAD names in the repository's refs, the first one git advertises.
func (p *Plain) Head(ctx context.Context, _, repository string) (string, error) {
	advertised, err := p.refs(ctx, repository)
	if err != nil {
		return "", err
	}
	if sha, ok := advertisedHead(advertised); ok {
		return sha, nil
	}
	return "", EmptyRepository(repository)
}

// maxAdvertisedHead is how much of the refs Head reads: HEAD comes first.
const maxAdvertisedHead = 64 << 10

// refs is the start of the host's answer to git's request for the repository's refs.
func (p *Plain) refs(ctx context.Context, repository string) ([]byte, error) {
	address := p.URL(repository)
	notReachable := func(why string) error {
		return api.Refuse(http.StatusUnprocessableEntity, "repository_not_reachable", "no repository answers git at "+address+" ("+why+"): it is created on the git host before a platform experiment names it")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		return nil, notReachable("the address is not one")
	}
	if p.username != "" {
		req.SetBasicAuth(p.username, p.password)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, notReachable("the host did not answer")
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, notReachable(fmt.Sprintf("HTTP %d", resp.StatusCode))
	}
	if resp.Header.Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
		return nil, notReachable("the answer is not git's")
	}
	advertised, err := io.ReadAll(io.LimitReader(resp.Body, maxAdvertisedHead))
	if err != nil {
		return nil, notReachable("the answer could not be read")
	}
	return advertised, nil
}

// advertisedHead is the commit of the HEAD line of a smart HTTP ref advertisement: pkt-lines, each
// four hex digits of length and then "<sha> <ref>", the first ref followed by NUL and the
// capabilities. A repository with no commit advertises no HEAD.
func advertisedHead(advertised []byte) (string, bool) {
	for len(advertised) >= 4 {
		length, err := strconv.ParseUint(string(advertised[:4]), 16, 16)
		if err != nil {
			return "", false
		}
		if length < 4 {
			advertised = advertised[4:]
			continue
		}
		if int(length) > len(advertised) {
			return "", false
		}
		line := strings.TrimSuffix(string(advertised[4:length]), "\n")
		advertised = advertised[length:]
		line, _, _ = strings.Cut(line, "\x00")
		if sha, ref, ok := strings.Cut(line, " "); ok && ref == "HEAD" {
			return sha, true
		}
	}
	return "", false
}

func (p *Plain) Create(context.Context, string, string, string, string, map[string]string) (string, error) {
	return "", api.Refuse(http.StatusUnprocessableEntity, "repository_required", "the platform creates no repository on this deployment's git host: create one under "+p.base.String()+" and name it in repository")
}

func (p *Plain) File(_ context.Context, _, _, path string) ([]byte, error) {
	return nil, api.Refuse(http.StatusUnprocessableEntity, "experiment_file_unsupported", "this deployment's git host hands the platform no file, so "+path+" cannot be read from the repository: state its fields in the request")
}

func (p *Plain) Credential(context.Context, string, string, bool) (*Credential, error) {
	if p.username == "" {
		return nil, api.Refuse(http.StatusNotFound, "not_found", "this deployment's git host takes no credential")
	}
	return &Credential{Username: p.username, Password: p.password}, nil
}

// Serves takes the repository's address with or without .git, or its LFS endpoint under it.
func (p *Plain) Serves(repository, protocol, host, path string) bool {
	asked := strings.TrimSuffix(strings.Trim(path, "/"), "/info/lfs")
	served := strings.Trim(p.base.Path+"/"+repository, "/")
	return protocol == p.base.Scheme && strings.EqualFold(host, p.base.Host) && strings.TrimSuffix(asked, ".git") == strings.TrimSuffix(served, ".git")
}
