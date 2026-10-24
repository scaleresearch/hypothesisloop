package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth/authtest"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/openrouter"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/models"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/platformexperiments/platformexperimentstest"
)

type openRouterListing struct {
	models []openrouter.Model
	err    error
}

func (l openRouterListing) Models(context.Context) ([]openrouter.Model, error) {
	return l.models, l.err
}

func modelsRouter(t *testing.T, caller auth.Principal, listing models.OpenRouter) http.Handler {
	t.Helper()
	r := authtest.Router(authtest.As(caller))
	doc := NewDoc(r, "test", "1.0.0", "", auth.NewAccess(sessionPlacement), zap.NewNop())
	registerModels(doc, models.New(platformexperimentstest.Catalog, listing, platformexperimentstest.Defaults), zap.NewNop())
	return r
}

// Every caller with a token reads the models the platform serves, each under its reference, and
// the defaults an agent and a coordinator that name none run on.
func TestEveryCallerListsTheModelsServedAndTheDefaults(t *testing.T) {
	listing := openRouterListing{models: []openrouter.Model{{ID: "moonshotai/kimi-k2", Name: "MoonshotAI: Kimi K2", ContextLength: 131072, InputUSDPerMTok: 0.6, OutputUSDPerMTok: 2.5}}}
	for _, caller := range []auth.Principal{
		{Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}},
		{Kind: auth.KindAgent, AgentID: "ag-1", PlatformExperimentID: "pe-1", OrgIDs: []string{"org-a"}},
		{Kind: auth.KindCoordinator, PlatformExperimentID: "pe-1", OrgIDs: []string{"org-a"}},
	} {
		code, out := call(t, modelsRouter(t, caller, listing), "GET", "/models", "")
		listed, _ := out["models"].([]any)
		if code != http.StatusOK || len(listed) != 4 || out["default_model"] != "anthropic/model-default" || out["coordinator_model"] != "anthropic/model-c" {
			t.Fatalf("%s: got = %d %v", caller.Kind, code, out)
		}
		kimi, _ := listed[3].(map[string]any)
		if kimi["model"] != "openrouter/moonshotai/kimi-k2" || kimi["provider"] != "openrouter" || kimi["name"] != "MoonshotAI: Kimi K2" ||
			kimi["context_length"] != 131072.0 || kimi["input_usd_per_mtok"] != 0.6 || kimi["output_usd_per_mtok"] != 2.5 {
			t.Errorf("%s: the openrouter entry = %v", caller.Kind, kimi)
		}
		first, _ := listed[0].(map[string]any)
		if first["model"] != "anthropic/model-2" || first["provider"] != "anthropic" || first["name"] != "model-2" || first["context_length"] != 0.0 {
			t.Errorf("%s: the first anthropic entry = %v", caller.Kind, first)
		}
	}
}

func TestAListingOpenRouterDoesNotAnswerIsTheServersFailure(t *testing.T) {
	r := modelsRouter(t, auth.Principal{Kind: auth.KindUser, UserID: "u-1", OrgIDs: []string{"org-a"}}, openRouterListing{err: errors.New("unreachable")})
	if code, out := call(t, r, "GET", "/models", ""); code != http.StatusInternalServerError {
		t.Fatalf("got = %d %v, want 500", code, out)
	}
}
