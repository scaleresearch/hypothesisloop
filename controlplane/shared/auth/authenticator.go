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

	"github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
)

// Error is a refusal with the HTTP status it answers with, in the API's {"error": ...} shape.
// Reason is set on the one refusal a client branches on (domain.ReasonClusterNotRegistered).
type Error struct {
	status  int
	Message string `json:"error"`
	Reason  string `json:"reason,omitempty"`
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
	EnsureUserHome(ctx context.Context, userID, name, orgName string) (*domain.User, []string, error)
	APIKeyByHash(ctx context.Context, keyHash string) (*domain.APIKey, error)
	PlatformExperimentTenancy(ctx context.Context, platformExperimentID string) (tenancy *domain.Tenancy, closedAt *time.Time, err error)
	ExperimentTenancy(ctx context.Context, experimentID string) (*domain.Tenancy, bool, error)
	AgentCutAt(ctx context.Context, platformExperimentID, agentID string) (*time.Time, error)
	AgentStanding(ctx context.Context, platformExperimentID, agentID string) (db.AgentStanding, error)
	ClusterOrg(ctx context.Context, name string) (orgID string, found bool, err error)
	AssignedEnvironmentBuild(ctx context.Context, cluster, key string, attempt int) (*domain.Environment, error)
}

// StoreDirectory is the Directory over the platform's own database.
type StoreDirectory struct {
	*db.AccountsStore
	*db.ClustersStore
	*db.PlatformExperimentsStore
	*db.EnvironmentsStore
}

func NewStoreDirectory(pool *db.Pool) StoreDirectory {
	return StoreDirectory{AccountsStore: db.NewAccountsStore(pool), ClustersStore: db.NewClustersStore(pool), PlatformExperimentsStore: db.NewPlatformExperimentsStore(pool), EnvironmentsStore: db.NewEnvironmentsStore(pool)}
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
			refusal := unauthenticated("cluster %s is not registered", p.ClusterName)
			refusal.Reason = domain.ReasonClusterNotRegistered
			return Principal{}, refusal
		}
		p.OrgIDs = []string{orgID}
	case KindBuild:
		env, err := a.directory.AssignedEnvironmentBuild(ctx, p.Build.Cluster, p.Build.Key, p.Build.Attempt)
		if err != nil {
			return Principal{}, err
		}
		if env == nil {
			return Principal{}, unauthenticated("the build of %s is not building on cluster %s at attempt %d", p.Build.Key, p.Build.Cluster, p.Build.Attempt)
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
		if a.woundDownAfter(closedAt, domain.CoordinatorWindDown) {
			return Principal{}, unauthenticated("platform experiment %s is closed", p.PlatformExperimentID)
		}
		p = p.within(tenancy)
		p.PlatformExperimentClosed = closedAt != nil
		p.RunOver = closedAt != nil
	case KindAgent:
		standing, err := a.directory.AgentStanding(ctx, p.PlatformExperimentID, p.AgentID)
		if err != nil {
			return Principal{}, err
		}
		switch {
		case standing.Tenancy == nil:
			return Principal{}, unauthenticated("platform experiment %s does not exist", p.PlatformExperimentID)
		case !standing.SignedUp:
			return Principal{}, unauthenticated("agent %s is not in platform experiment %s", p.AgentID, p.PlatformExperimentID)
		case a.woundDown(standing.ClosedAt):
			return Principal{}, unauthenticated("platform experiment %s is closed", p.PlatformExperimentID)
		case a.woundDown(standing.CutAt):
			return Principal{}, unauthenticated("agent %s was cut from platform experiment %s", p.AgentID, p.PlatformExperimentID)
		case a.woundDown(standing.PlannerAnsweredAt):
			return Principal{}, unauthenticated("agent %s planned platform experiment %s and its planning is over", p.AgentID, p.PlatformExperimentID)
		case a.woundDown(standing.FinishedAt):
			return Principal{}, unauthenticated("agent %s finished in platform experiment %s", p.AgentID, p.PlatformExperimentID)
		}
		p = p.within(standing.Tenancy)
		p.RunOver = standing.ClosedAt != nil || standing.CutAt != nil || standing.PlannerAnsweredAt != nil || standing.FinishedAt != nil
	}
	return p, nil
}

// woundDown reports whether a token's time after its run ended has run out. An agent's run ends
// when its platform experiment closed, it was cut, it finished, or for a planner its answer to
// its draft was stored. A coordinator's ends when its platform experiment closed, and its token
// works for domain.CoordinatorWindDown after.
func (a *Authenticator) woundDown(endedAt *time.Time) bool {
	return a.woundDownAfter(endedAt, domain.AgentWindDown)
}

func (a *Authenticator) woundDownAfter(endedAt *time.Time, windDown time.Duration) bool {
	return endedAt != nil && !a.now().Before(endedAt.Add(windDown))
}

func (p Principal) within(t *domain.Tenancy) Principal {
	p.PlatformExperimentID, p.ProjectID, p.OrgIDs, p.PriorRuns = t.PlatformExperimentID, t.ProjectID, []string{t.OrgID}, t.PriorRuns
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
	// The name is the person's own claim and is kept to show beside what they do, so it is held
	// to what a name may be.
	user, orgIDs, err := a.directory.EnsureUserHome(ctx, claims.Subject, domain.NameFrom(claims.displayName()), domain.DefaultOrgName)
	if err != nil {
		return Principal{}, err
	}
	p := Principal{Kind: KindUser, UserID: user.ID, Name: user.Name, OrgIDs: orgIDs}
	if user.Staff {
		p.Kind = KindStaff
	}
	return p, nil
}

// InternalError is the body of every answer to a failure of the platform's own. What failed is
// logged and never sent: the caller may be anyone.
const InternalError = "internal error"

// Internal logs a failure of the platform's own, with what the request was doing, and returns the
// 500 that says nothing about it. Every handler answers such a failure through it. A failure that
// says which dependency was down (Unavailable) is answered 503 with that reason instead.
func Internal(logger *zap.Logger, what string, err error, fields ...zap.Field) *Error {
	logger.Error(what, append(fields, zap.Error(err))...)
	var down interface {
		Unavailable() (reason, message string)
	}
	if errors.As(err, &down) {
		reason, message := down.Unavailable()
		return &Error{status: http.StatusServiceUnavailable, Message: message, Reason: reason}
	}
	return &Error{status: http.StatusInternalServerError, Message: InternalError}
}

// WriteError answers a refusal in the API's {"error": ...} shape; any other error is logged and
// answered as a 500 that says nothing about it.
func WriteError(w http.ResponseWriter, logger *zap.Logger, err error) {
	var refusal *Error
	if !errors.As(err, &refusal) {
		refusal = Internal(logger, "request failed before its handler", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(refusal.status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(refusal)
}
