package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Error is a refusal with the HTTP status it answers with, in the API's {"error": ...} shape.
type Error struct {
	status  int
	Message string `json:"error"`
}

func (e *Error) Error() string  { return e.Message }
func (e *Error) GetStatus() int { return e.status }

// Unauthenticated is the refusal of a request that carries no usable token.
func Unauthenticated(message string) *Error { return unauthenticated("%s", message) }

// Forbidden is the refusal of an authenticated caller.
func Forbidden(message string) *Error { return forbidden(message) }

func unauthenticated(format string, args ...any) *Error {
	return &Error{status: http.StatusUnauthorized, Message: "unauthenticated: " + fmt.Sprintf(format, args...)}
}

// Directory is what the authenticator reads to learn whether the thing a token names still
// stands. *db.Store with *db.AccountsStore and *db.ClustersStore satisfy it together.
type Directory interface {
	EnsureUserHome(ctx context.Context, userID, orgName string) (*domain.User, []string, error)
	APIKeyByHash(ctx context.Context, keyHash string) (*domain.APIKey, error)
	PlatformExperimentTenancy(ctx context.Context, platformExperimentID string) (tenancy *domain.Tenancy, closedAt *time.Time, err error)
	ExperimentTenancy(ctx context.Context, experimentID string) (*domain.Tenancy, bool, error)
	AgentCutAt(ctx context.Context, platformExperimentID, agentID string) (*time.Time, error)
	IsSignedUp(ctx context.Context, platformExperimentID, agentID string) (bool, error)
	ClusterOrg(ctx context.Context, name string) (orgID string, found bool, err error)
	LauncherKnown(ctx context.Context, name string) (bool, error)
}

// StoreDirectory is the Directory over the platform's own database.
type StoreDirectory struct {
	*db.AccountsStore
	*db.ClustersStore
	*db.PlatformExperimentsStore
}

func NewStoreDirectory(pool *db.Pool) StoreDirectory {
	return StoreDirectory{AccountsStore: db.NewAccountsStore(pool), ClustersStore: db.NewClustersStore(pool), PlatformExperimentsStore: db.NewPlatformExperimentsStore(pool)}
}

// Authenticator turns a token into a Principal. Nothing is kept between requests: a derived
// token is valid while the row it names says so, an API key while it is not revoked.
type Authenticator struct {
	signingSecret string
	providers     map[string]identityProvider
	directory     Directory
	now           func() time.Time
}

func NewAuthenticator(cfg config.AuthConfig, directory Directory) (*Authenticator, error) {
	providers, err := parseIdentityProviders(cfg.IdentityProviders)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return &Authenticator{signingSecret: cfg.SigningSecret, providers: providers, directory: directory, now: time.Now}, nil
}

// BearerToken returns the token of an Authorization header.
func BearerToken(header string) (string, error) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return "", unauthenticated("send Authorization: Bearer <token>")
	}
	return token, nil
}

// Authenticate resolves a token. A refusal is an *Error with status 401; any other error is the
// platform failing to answer.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (Principal, error) {
	switch {
	case strings.HasPrefix(token, derivedPrefix):
		return a.derived(ctx, token)
	case strings.HasPrefix(token, apiKeyPrefix):
		return a.apiKey(ctx, token)
	default:
		return a.user(ctx, token)
	}
}

func (a *Authenticator) derived(ctx context.Context, token string) (Principal, error) {
	p, err := verifyDerived(a.signingSecret, token)
	if err != nil {
		return Principal{}, unauthenticated("%s", err)
	}
	switch p.Kind {
	case KindRuntime:
		orgID, found, err := a.directory.ClusterOrg(ctx, p.ClusterName)
		if err != nil {
			return Principal{}, err
		}
		if !found {
			return Principal{}, unauthenticated("cluster %s is not registered", p.ClusterName)
		}
		p.OrgIDs = []string{orgID}
	case KindLauncher:
		known, err := a.directory.LauncherKnown(ctx, p.LauncherName)
		if err != nil {
			return Principal{}, err
		}
		if !known {
			return Principal{}, unauthenticated("launcher %s is not registered", p.LauncherName)
		}
	case KindJob:
		tenancy, ended, err := a.directory.ExperimentTenancy(ctx, p.ExperimentID)
		if err != nil {
			return Principal{}, err
		}
		if tenancy == nil || ended {
			return Principal{}, unauthenticated("job %s is not running", p.ExperimentID)
		}
		_, closedAt, err := a.directory.PlatformExperimentTenancy(ctx, tenancy.PlatformExperimentID)
		if err != nil {
			return Principal{}, err
		}
		cutAt, err := a.directory.AgentCutAt(ctx, tenancy.PlatformExperimentID, tenancy.AgentID)
		if err != nil {
			return Principal{}, err
		}
		p = p.within(tenancy)
		p.RunOver = closedAt != nil || cutAt != nil
	case KindCoordinator:
		tenancy, closedAt, err := a.directory.PlatformExperimentTenancy(ctx, p.PlatformExperimentID)
		if err != nil {
			return Principal{}, err
		}
		if tenancy == nil {
			return Principal{}, unauthenticated("platform experiment %s does not exist", p.PlatformExperimentID)
		}
		p = p.within(tenancy)
		p.PlatformExperimentClosed = closedAt != nil
		p.RunOver = closedAt != nil
	case KindAgent:
		tenancy, closedAt, err := a.directory.PlatformExperimentTenancy(ctx, p.PlatformExperimentID)
		if err != nil {
			return Principal{}, err
		}
		if tenancy == nil {
			return Principal{}, unauthenticated("platform experiment %s does not exist", p.PlatformExperimentID)
		}
		signedUp, err := a.directory.IsSignedUp(ctx, p.PlatformExperimentID, p.AgentID)
		if err != nil {
			return Principal{}, err
		}
		if !signedUp {
			return Principal{}, unauthenticated("agent %s is not in platform experiment %s", p.AgentID, p.PlatformExperimentID)
		}
		if a.woundDown(closedAt) {
			return Principal{}, unauthenticated("platform experiment %s is closed", p.PlatformExperimentID)
		}
		cutAt, err := a.directory.AgentCutAt(ctx, p.PlatformExperimentID, p.AgentID)
		if err != nil {
			return Principal{}, err
		}
		if a.woundDown(cutAt) {
			return Principal{}, unauthenticated("agent %s was cut from platform experiment %s", p.AgentID, p.PlatformExperimentID)
		}
		p = p.within(tenancy)
		p.RunOver = closedAt != nil || cutAt != nil
	}
	return p, nil
}

// woundDown reports whether an agent's time after its platform experiment closed, or after it
// was cut, has run out.
func (a *Authenticator) woundDown(endedAt *time.Time) bool {
	return endedAt != nil && !a.now().Before(endedAt.Add(domain.AgentWindDown))
}

func (p Principal) within(t *domain.Tenancy) Principal {
	p.PlatformExperimentID, p.ProjectID, p.OrgIDs = t.PlatformExperimentID, t.ProjectID, []string{t.OrgID}
	return p
}

func (a *Authenticator) apiKey(ctx context.Context, token string) (Principal, error) {
	key, err := a.directory.APIKeyByHash(ctx, HashAPIKey(token))
	if err != nil {
		return Principal{}, err
	}
	if key == nil {
		return Principal{}, unauthenticated("unknown api key")
	}
	if key.RevokedAt != nil {
		return Principal{}, unauthenticated("api key %s is revoked, or was minted from a revoked key", key.ID)
	}
	if key.Orphaned {
		return Principal{}, unauthenticated("api key %s outlived its creator's hold on the org", key.ID)
	}
	return Principal{Kind: KindKey, KeyID: key.ID, OrgIDs: []string{key.OrgID}, ProjectID: key.ProjectID}, nil
}

func (a *Authenticator) user(ctx context.Context, token string) (Principal, error) {
	claims, err := verifyJWT(token, a.providers, a.now())
	if err != nil {
		return Principal{}, unauthenticated("%s", err)
	}
	name := claims.displayName()
	user, orgIDs, err := a.directory.EnsureUserHome(ctx, claims.Subject, domain.DefaultOrgName)
	if err != nil {
		return Principal{}, err
	}
	p := Principal{Kind: KindUser, UserID: user.ID, Name: name, OrgIDs: orgIDs}
	if user.Staff {
		p.Kind = KindStaff
	}
	return p, nil
}

// RequireRuntime serves next only to a runtime token of a registered cluster, or to staff, with
// the principal in the request's context. It guards the plain HTTP endpoints runtimes push to
// outside the documented API.
func (a *Authenticator) RequireRuntime(logger *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.AuthenticateRequest(r)
		if err == nil && p.Kind != KindRuntime && p.Kind != KindStaff {
			err = forbidden("forbidden: only a runtime token may push here")
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// RequireStaff serves next only to staff.
func (a *Authenticator) RequireStaff(logger *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.AuthenticateRequest(r)
		if err == nil && p.Kind != KindStaff {
			err = forbidden("forbidden: staff only")
		}
		if err != nil {
			WriteError(w, logger, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AuthenticateRequest resolves the bearer token of a plain HTTP request.
func (a *Authenticator) AuthenticateRequest(r *http.Request) (Principal, error) {
	token, err := BearerToken(r.Header.Get("Authorization"))
	if err != nil {
		return Principal{}, err
	}
	return a.Authenticate(r.Context(), token)
}

// InternalError is the body of every answer to a failure of the platform's own. What failed is
// logged and never sent: the caller may be anyone.
const InternalError = "internal error"

// WriteError answers a refusal in the API's {"error": ...} shape; any other error is logged and
// answered as a 500 that says nothing about it.
func WriteError(w http.ResponseWriter, logger *zap.Logger, err error) {
	var refusal *Error
	if !errors.As(err, &refusal) {
		logger.Error("request failed before its handler", zap.Error(err))
		refusal = &Error{status: http.StatusInternalServerError, Message: InternalError}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(refusal.status)
	_ = json.NewEncoder(w).Encode(refusal)
}
