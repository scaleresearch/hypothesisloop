package gcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// MinLifetime is how long every login handed out is still good for: a build pushes, and a node
// pulls, well within it.
const MinLifetime = 25 * time.Minute

// tokenLifetime is how long a minted token lives, the longest Google grants without an org
// policy that extends it.
const tokenLifetime = time.Hour

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// username is what Artifact Registry takes an access token under.
const username = "oauth2accesstoken"

// minter returns a fresh access token of serviceAccount.
type minter func(ctx context.Context, serviceAccount string) (*oauth2.Token, error)

// Source hands out the logins of one registry.
type Source struct {
	registry       string
	pusher, puller *reused
}

// New impersonates pusher and puller, as the process's Google application default credentials,
// for logins to the registry repository is in. It fails when there are no such credentials.
func New(ctx context.Context, repository, pusher, puller string) (*Source, error) {
	credentials, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("registrycred gcp: Google application default credentials: %w", err)
	}
	return newSource(repository, pusher, puller, impersonated(credentials.TokenSource), time.Now)
}

func newSource(repository, pusher, puller string, mint minter, now func() time.Time) (*Source, error) {
	registry, _, ok := strings.Cut(repository, "/")
	if !ok || registry == "" || pusher == "" || puller == "" {
		return nil, errors.New("registrycred gcp: a repository host/path and both service accounts are required")
	}
	return &Source{
		registry: registry,
		pusher:   &reused{account: pusher, mint: mint, now: now},
		puller:   &reused{account: puller, mint: mint, now: now},
	}, nil
}

// impersonated mints a token of a service account as base, a new one on each call.
func impersonated(base oauth2.TokenSource) minter {
	return func(ctx context.Context, serviceAccount string) (*oauth2.Token, error) {
		// A stated lifetime makes the source mint once, here, and never again.
		source, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: serviceAccount,
			Scopes:          []string{cloudPlatformScope},
			Lifetime:        tokenLifetime,
		}, option.WithTokenSource(base))
		if err != nil {
			return nil, err
		}
		return source.Token()
	}
}

// Push is a login that writes to the registry.
func (s *Source) Push(ctx context.Context) (domain.RegistryLogin, error) {
	return s.login(ctx, s.pusher)
}

// Pull is a login that reads from the registry.
func (s *Source) Pull(ctx context.Context) (domain.RegistryLogin, error) {
	return s.login(ctx, s.puller)
}

func (s *Source) login(ctx context.Context, account *reused) (domain.RegistryLogin, error) {
	token, err := account.token(ctx)
	if err != nil {
		return domain.RegistryLogin{}, err
	}
	return domain.RegistryLogin{Registry: s.registry, Username: username, Password: token.AccessToken, ExpiresAt: token.Expiry}, nil
}

// reused hands out one service account's token until it has less than MinLifetime left, so a
// login is minted about once in tokenLifetime-MinLifetime and never more often.
type reused struct {
	account string
	mint    minter
	now     func() time.Time

	mu      sync.Mutex
	current *oauth2.Token
}

func (r *reused) token(ctx context.Context) (*oauth2.Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current != nil && r.current.Expiry.Sub(r.now()) >= MinLifetime {
		return r.current, nil
	}
	token, err := r.mint(ctx, r.account)
	if err != nil {
		return nil, fmt.Errorf("registrycred gcp: a token of %s: %w", r.account, err)
	}
	if left := token.Expiry.Sub(r.now()); left < MinLifetime {
		return nil, fmt.Errorf("registrycred gcp: a fresh token of %s lives %s, less than %s", r.account, left.Round(time.Second), MinLifetime)
	}
	r.current = token
	return token, nil
}
