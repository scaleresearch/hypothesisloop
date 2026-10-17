// Artifact Registry grants roles on a project or on one repository, nothing narrower: IAM
// conditions do not reach an image inside a repository, and credential access boundaries
// downscope Cloud Storage tokens only. So each org has a repository of its own, written by one
// service account and read by another, both its alone, and every login is a short-lived token of
// one of them. Federating the platform's own tokens with workload identity, a principal per org
// bound to the org's repository, would need no service accounts; it is a later provider.
package gcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/artifactregistry/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iam/v1"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// MinLifetime is how long every login handed out is still good for: a push of a large image, and
// a node's pull, end well within it.
const MinLifetime = 25 * time.Minute

// pullLifetime is how long a pull token lives, the longest Google grants without an org policy
// that extends it. A pull token is reused by every job of its org, until it has less than
// MinLifetime left.
const pullLifetime = time.Hour

// pushLifetime is how long a push token lives. Each push login is minted for its request alone
// and only while its run spends, so it outlives the run by at most this.
const pushLifetime = MinLifetime + 5*time.Minute

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// username is what Artifact Registry takes an access token under.
const username = "oauth2accesstoken"

// repositoryPrefix starts the id of every org's repository, and nothing else of the project.
const repositoryPrefix = "hl-"

const (
	writerRole = "roles/artifactregistry.writer"
	readerRole = "roles/artifactregistry.reader"
)

// minter returns a fresh access token of serviceAccount that lives lifetime.
type minter func(ctx context.Context, serviceAccount string, lifetime time.Duration) (*oauth2.Token, error)

// Registry is the Artifact Registry repositories of orgs in one project and location.
type Registry struct {
	project, location string
	ar                *artifactregistry.Service
	iam               *iam.Service
	mint              minter
	now               func() time.Time

	mu     sync.Mutex
	tokens map[string]*oauth2.Token
}

// New reaches project's Artifact Registry in location as the process's Google application
// default credentials, which create each org's repository and service accounts and mint their
// tokens. It fails when there are no such credentials.
func New(ctx context.Context, project, location string) (*Registry, error) {
	credentials, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("registrycred gcp: Google application default credentials: %w", err)
	}
	ar, err := artifactregistry.NewService(ctx, option.WithTokenSource(credentials.TokenSource))
	if err != nil {
		return nil, fmt.Errorf("registrycred gcp: artifact registry client: %w", err)
	}
	accounts, err := iam.NewService(ctx, option.WithTokenSource(credentials.TokenSource))
	if err != nil {
		return nil, fmt.Errorf("registrycred gcp: iam client: %w", err)
	}
	return &Registry{project: project, location: location, ar: ar, iam: accounts, mint: impersonated(credentials.TokenSource), now: time.Now, tokens: map[string]*oauth2.Token{}}, nil
}

// impersonated mints a token of a service account as base, a new one on each call.
func impersonated(base oauth2.TokenSource) minter {
	return func(ctx context.Context, serviceAccount string, lifetime time.Duration) (*oauth2.Token, error) {
		// A stated lifetime makes the source mint once, here, and never again.
		source, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: serviceAccount,
			Scopes:          []string{cloudPlatformScope},
			Lifetime:        lifetime,
		}, option.WithTokenSource(base))
		if err != nil {
			return nil, err
		}
		return source.Token()
	}
}

func (r *Registry) host() string { return r.location + "-docker.pkg.dev" }

// orgKey names orgID in ids that allow fewer characters than an org id holds.
func orgKey(orgID string) string {
	sum := sha256.Sum256([]byte(orgID))
	return hex.EncodeToString(sum[:])[:24]
}

func (r *Registry) repositoryID(orgID string) string { return repositoryPrefix + orgKey(orgID) }

func (r *Registry) repositoryName(orgID string) string {
	return "projects/" + r.project + "/locations/" + r.location + "/repositories/" + r.repositoryID(orgID)
}

func (r *Registry) pusher(orgID string) string { return r.account("hlw-", orgID) }
func (r *Registry) puller(orgID string) string { return r.account("hlr-", orgID) }

func (r *Registry) account(prefix, orgID string) string {
	return prefix + orgKey(orgID) + "@" + r.project + ".iam.gserviceaccount.com"
}

// Repository is where the images of platform experiment peID of orgID are pushed.
func (r *Registry) Repository(orgID, peID string) string {
	return r.host() + "/" + r.project + "/" + r.repositoryID(orgID) + "/" + peID
}

// Holds says image is in the repository of orgID.
func (r *Registry) Holds(orgID, image string) bool {
	return strings.HasPrefix(image, r.host()+"/"+r.project+"/"+r.repositoryID(orgID)+"/")
}

// Managed says image is in the repository of some org.
func (r *Registry) Managed(image string) bool {
	return strings.HasPrefix(image, r.host()+"/"+r.project+"/"+repositoryPrefix)
}

// PushLogin is a login that writes to and reads from the repository of orgID, which Provision
// made, minted for this call alone.
func (r *Registry) PushLogin(ctx context.Context, orgID string) (domain.RegistryLogin, error) {
	token, err := r.fresh(ctx, r.pusher(orgID), pushLifetime)
	if err != nil {
		return domain.RegistryLogin{}, err
	}
	return r.loginOf(token), nil
}

// PullLogin is the login a job of orgID pulls image with: a read-only one of the org's repository
// for an image in it, and nil for any other, which is pulled without one.
func (r *Registry) PullLogin(ctx context.Context, orgID, image string) (*domain.RegistryLogin, error) {
	if !r.Holds(orgID, image) {
		return nil, nil
	}
	token, err := r.token(ctx, r.puller(orgID))
	if err != nil {
		return nil, err
	}
	login := r.loginOf(token)
	return &login, nil
}

func (r *Registry) loginOf(token *oauth2.Token) domain.RegistryLogin {
	return domain.RegistryLogin{Registry: r.host(), Username: username, Password: token.AccessToken, ExpiresAt: token.Expiry}
}

// token hands out one puller's token until it has less than MinLifetime left, so it is minted
// about once in pullLifetime-MinLifetime and never more often.
func (r *Registry) token(ctx context.Context, account string) (*oauth2.Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current := r.tokens[account]; current != nil && current.Expiry.Sub(r.now()) >= MinLifetime {
		return current, nil
	}
	token, err := r.fresh(ctx, account, pullLifetime)
	if err != nil {
		return nil, err
	}
	r.tokens[account] = token
	return token, nil
}

// fresh mints a token of account that lives lifetime, and refuses one that has less than
// MinLifetime left.
func (r *Registry) fresh(ctx context.Context, account string, lifetime time.Duration) (*oauth2.Token, error) {
	token, err := r.mint(ctx, account, lifetime)
	if err != nil {
		return nil, fmt.Errorf("registrycred gcp: a token of %s: %w", account, err)
	}
	if left := token.Expiry.Sub(r.now()); left < MinLifetime {
		return nil, fmt.Errorf("registrycred gcp: a fresh token of %s lives %s, less than %s", account, left.Round(time.Second), MinLifetime)
	}
	return token, nil
}

// Provision makes the repository of orgID and its two service accounts exist, the pusher a writer
// of it and the puller a reader. Each step leaves what is there as it is, so a provisioning that
// failed part way is finished by the next. IAM learns of a service account just created
// eventually, so the binding of one may fail until it has.
func (r *Registry) Provision(ctx context.Context, orgID string) error {
	if err := r.ensureRepository(ctx, orgID); err != nil {
		return err
	}
	for _, account := range []string{r.pusher(orgID), r.puller(orgID)} {
		if err := r.ensureAccount(ctx, account, orgID); err != nil {
			return err
		}
	}
	return r.ensureBindings(ctx, orgID)
}

func (r *Registry) ensureRepository(ctx context.Context, orgID string) error {
	name := r.repositoryName(orgID)
	_, err := r.ar.Projects.Locations.Repositories.Get(name).Context(ctx).Do()
	if err == nil {
		return nil
	}
	if !hasStatus(err, http.StatusNotFound) {
		return fmt.Errorf("registrycred gcp: read repository %s: %w", name, err)
	}
	op, err := r.ar.Projects.Locations.Repositories.Create("projects/"+r.project+"/locations/"+r.location, &artifactregistry.Repository{
		Format:      "DOCKER",
		Description: "HypothesisLoop images of org " + orgID,
	}).RepositoryId(r.repositoryID(orgID)).Context(ctx).Do()
	if hasStatus(err, http.StatusConflict) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("registrycred gcp: create repository %s: %w", name, err)
	}
	for !op.Done {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if op, err = r.ar.Projects.Locations.Operations.Get(op.Name).Context(ctx).Do(); err != nil {
			return fmt.Errorf("registrycred gcp: create repository %s: %w", name, err)
		}
	}
	if op.Error != nil {
		return fmt.Errorf("registrycred gcp: create repository %s: %s", name, op.Error.Message)
	}
	return nil
}

// ensureAccount reads the account before it creates one: every create counts against a small
// per-minute project quota, the refused ones included, and a provisioning is tried again until
// it succeeds.
func (r *Registry) ensureAccount(ctx context.Context, email, orgID string) error {
	_, err := r.iam.Projects.ServiceAccounts.Get("projects/" + r.project + "/serviceAccounts/" + email).Context(ctx).Do()
	if err == nil {
		return nil
	}
	if !hasStatus(err, http.StatusNotFound) {
		return fmt.Errorf("registrycred gcp: read service account %s: %w", email, err)
	}
	id, _, _ := strings.Cut(email, "@")
	_, err = r.iam.Projects.ServiceAccounts.Create("projects/"+r.project, &iam.CreateServiceAccountRequest{
		AccountId:      id,
		ServiceAccount: &iam.ServiceAccount{DisplayName: "HypothesisLoop images of org " + orgID},
	}).Context(ctx).Do()
	if err != nil && !hasStatus(err, http.StatusConflict) {
		return fmt.Errorf("registrycred gcp: create service account %s: %w", email, err)
	}
	return nil
}

func (r *Registry) ensureBindings(ctx context.Context, orgID string) error {
	name := r.repositoryName(orgID)
	policy, err := r.ar.Projects.Locations.Repositories.GetIamPolicy(name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("registrycred gcp: read the policy of %s: %w", name, err)
	}
	changed := false
	for role, account := range map[string]string{writerRole: r.pusher(orgID), readerRole: r.puller(orgID)} {
		if bind(policy, role, "serviceAccount:"+account) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if _, err := r.ar.Projects.Locations.Repositories.SetIamPolicy(name, &artifactregistry.SetIamPolicyRequest{Policy: policy}).Context(ctx).Do(); err != nil {
		return fmt.Errorf("registrycred gcp: write the policy of %s: %w", name, err)
	}
	return nil
}

// bind adds member to role in policy, and says whether it was not there.
func bind(policy *artifactregistry.Policy, role, member string) bool {
	for _, binding := range policy.Bindings {
		if binding.Role == role && binding.Condition == nil {
			if slices.Contains(binding.Members, member) {
				return false
			}
			binding.Members = append(binding.Members, member)
			return true
		}
	}
	policy.Bindings = append(policy.Bindings, &artifactregistry.Binding{Role: role, Members: []string{member}})
	return true
}

func hasStatus(err error, code int) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == code
}
