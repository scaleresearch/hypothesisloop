package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	PlatformExperimentTenancy(ctx context.Context, platformExperimentID string) (*domain.Tenancy, bool, error)
	ExperimentTenancy(ctx context.Context, experimentID string) (*domain.Tenancy, bool, error)
	IsAgentCut(ctx context.Context, platformExperimentID, agentID string) (bool, error)
	ClusterOrg(ctx context.Context, name string) (orgID string, found bool, err error)
}

// StoreDirectory is the Directory over the platform's own database.
type StoreDirectory struct {
	*db.AccountsStore
	*db.ClustersStore
	*db.PlatformExperimentsStore
}

func NewStoreDirectory(pool *db.Pool) StoreDirectory {
	return StoreDirectory{db.NewAccountsStore(pool), db.NewClustersStore(pool), db.NewPlatformExperimentsStore(pool)}
}

// Authenticator turns a token into a Principal. Nothing is kept between requests: a derived
// token is valid while the row it names says so, an API key while it is not revoked.
type Authenticator struct {
	signingSecret string
	issuer        string
	keys          map[string]verifyingKey
	directory     Directory
	now           func() time.Time
}

func NewAuthenticator(cfg config.AuthConfig, directory Directory) (*Authenticator, error) {
	keys := make(map[string]verifyingKey, len(cfg.JWTKeys))
	for _, k := range cfg.JWTKeys {
		parsed, err := parseJWK(k)
		if err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
		if _, dup := keys[k.Kid]; dup {
			return nil, fmt.Errorf("auth: jwt key %s is listed twice", k.Kid)
		}
		keys[k.Kid] = parsed
	}
	return &Authenticator{signingSecret: cfg.SigningSecret, issuer: cfg.Issuer, keys: keys, directory: directory, now: time.Now}, nil
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
	case KindJob:
		tenancy, ended, err := a.directory.ExperimentTenancy(ctx, p.ExperimentID)
		if err != nil {
			return Principal{}, err
		}
		if tenancy == nil || ended {
			return Principal{}, unauthenticated("job %s is not running", p.ExperimentID)
		}
		p = p.within(tenancy)
	case KindAgent, KindCoordinator:
		tenancy, closed, err := a.directory.PlatformExperimentTenancy(ctx, p.PlatformExperimentID)
		if err != nil {
			return Principal{}, err
		}
		if tenancy == nil || closed {
			return Principal{}, unauthenticated("platform experiment %s is closed or does not exist", p.PlatformExperimentID)
		}
		if p.Kind == KindAgent {
			cut, err := a.directory.IsAgentCut(ctx, p.PlatformExperimentID, p.AgentID)
			if err != nil {
				return Principal{}, err
			}
			if cut {
				return Principal{}, unauthenticated("agent %s was cut from platform experiment %s", p.AgentID, p.PlatformExperimentID)
			}
		}
		p = p.within(tenancy)
	}
	return p, nil
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
		return Principal{}, unauthenticated("api key %s is revoked", key.ID)
	}
	return Principal{Kind: KindKey, OrgIDs: []string{key.OrgID}, ProjectID: key.ProjectID}, nil
}

func (a *Authenticator) user(ctx context.Context, token string) (Principal, error) {
	subject, name, err := verifyJWT(token, a.issuer, a.keys, a.now())
	if err != nil {
		return Principal{}, unauthenticated("%s", err)
	}
	orgName := name
	if orgName == "" {
		orgName = subject
	}
	user, orgIDs, err := a.directory.EnsureUserHome(ctx, subject, orgName)
	if err != nil {
		return Principal{}, err
	}
	p := Principal{Kind: KindUser, UserID: user.ID, Name: name, OrgIDs: orgIDs}
	if user.Staff {
		p.Kind = KindStaff
	}
	return p, nil
}

// RequireRuntime serves next only to a runtime token of a registered cluster, or to staff. It
// guards the plain HTTP endpoints runtimes push to outside the documented API.
func (a *Authenticator) RequireRuntime(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := BearerToken(r.Header.Get("Authorization"))
		if err == nil {
			var p Principal
			if p, err = a.Authenticate(r.Context(), token); err == nil && p.Kind != KindRuntime && p.Kind != KindStaff {
				err = forbidden("forbidden: only a runtime token may push here")
			}
		}
		if err != nil {
			WriteError(w, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WriteError answers a refusal in the API's {"error": ...} shape; any other error is a 500.
func WriteError(w http.ResponseWriter, err error) {
	var refusal *Error
	if !errors.As(err, &refusal) {
		refusal = &Error{status: http.StatusInternalServerError, Message: "could not authenticate the request: " + err.Error()}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(refusal.status)
	_ = json.NewEncoder(w).Encode(refusal)
}
