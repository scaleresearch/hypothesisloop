package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/accounts"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

func (a accountsAPI) registerAPIKeys(doc *apidocs.Doc) {
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "create-api-key", Method: "POST", Path: "/orgs/{id}/api-keys", DefaultStatus: http.StatusCreated,
		Summary: "Create an API key", Tags: []string{"accounts"},
		Description: "Returns the token once: the platform keeps its hash only, so a lost token is replaced by a new key. " +
			"It is the one secret of this API that cannot be read again; a cluster's runtime token and an agent's token are derived and are returned whenever asked. " +
			"With project_id the key reaches that project only, without it the whole org. An owner creates either; a member one bound to a project. " +
			"A key works while the person who created it could still create it: it stops when they leave the org, and one of the whole org when they stop being an owner. " +
			"A key may mint keys no wider than itself, and revoking a key revokes every key minted from it.",
	}, a.createAPIKey)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "list-api-keys", Method: "GET", Path: "/orgs/{id}/api-keys",
		Summary: "List an org's API keys", Tags: []string{"accounts"},
		Description: "Newest first, revoked ones included. No token is ever listed. Every member sees them all. An API key sees itself and the keys minted from it.",
	}, a.listAPIKeys)
	accountsRoute(doc, a.accounts, huma.Operation{
		OperationID: "revoke-api-key", Method: "DELETE", Path: "/api-keys/{id}",
		Summary: "Revoke an API key", Tags: []string{"accounts"},
		Description: "The key and every key minted from it stop working at once. Revoking a revoked key changes nothing. " +
			"An owner revokes any key of the org, a member the keys they created, an API key itself and the keys minted from it.",
	}, a.revokeAPIKey)
}

func (a accountsAPI) createAPIKey(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body struct {
		Name      string `json:"name"`
		ProjectID string `json:"project_id,omitempty"`
	}
}) (*struct{ Body accounts.CreatedAPIKey }, error) {
	created, err := a.accounts.CreateAPIKey(ctx, in.ID, in.Body.ProjectID, in.Body.Name)
	if err != nil {
		return nil, err
	}
	return &struct{ Body accounts.CreatedAPIKey }{Body: *created}, nil
}

func (a accountsAPI) listAPIKeys(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct {
	Body struct {
		APIKeys []*domain.APIKey `json:"api_keys"`
	}
}, error) {
	keys, err := a.accounts.APIKeys(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	out := &struct {
		Body struct {
			APIKeys []*domain.APIKey `json:"api_keys"`
		}
	}{}
	out.Body.APIKeys = keys
	return out, nil
}

func (a accountsAPI) revokeAPIKey(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body domain.APIKey }, error) {
	revoked, err := a.accounts.RevokeAPIKey(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return &struct{ Body domain.APIKey }{Body: *revoked}, nil
}
