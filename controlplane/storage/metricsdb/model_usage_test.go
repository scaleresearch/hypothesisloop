package metricsdb

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestRecordModelCallInsertsOneRowWithItsLabelsQuoted(t *testing.T) {
	var statements []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		statements = append(statements, r.PostForm.Get("sql"))
		_, _ = w.Write([]byte(`{"output":[{"affectedrows":1}]}`))
	}))
	defer server.Close()

	at := time.UnixMilli(1_700_000_000_123)
	err := New(server.URL).RecordModelCall(context.Background(), ModelCall{
		OrgID: "org-1", PlatformExperimentID: "pe-1", AgentID: "it's-a", Provider: "anthropic", Model: "claude-haiku-5-5",
		InputTokens: 10, OutputTokens: 20, CacheCreationInputTokens: 30, CacheReadInputTokens: 40, CostUSD: 0.0000504, At: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `INSERT INTO model_calls (org_id, platform_experiment_id, agent_id, provider, model, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens, cost_usd, ts) VALUES ('org-1', 'pe-1', 'it''s-a', 'anthropic', 'claude-haiku-5-5', 10, 20, 30, 40, 5.04e-05, 1700000000123)`
	if len(statements) != 1 || statements[0] != want {
		t.Fatalf("statements = %q, want one %q", statements, want)
	}
}

func TestRecordModelCallRefusesACallNamingNoOwner(t *testing.T) {
	if err := New("http://unused.invalid").RecordModelCall(context.Background(), ModelCall{PlatformExperimentID: "pe-1", AgentID: "a", Provider: "anthropic"}); err == nil {
		t.Fatal("a call with no org was recorded")
	}
}

func TestModelUsageOfSumsPerAgentAndModel(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		queries = append(queries, r.PostForm.Get("sql"))
		_, _ = w.Write([]byte(`{"output":[{"records":{"rows":[["agent-a","anthropic","claude-haiku-5-5",2,15,7,100,2000,0.25],["coordinator","openrouter","moonshotai/kimi-k2",1,1,2,3,4,0.0000504]]}}]}`))
	}))
	defer server.Close()

	got, err := New(server.URL).ModelUsageOf(context.Background(), "pe-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], "SUM(cost_usd)") || !strings.Contains(queries[0], "FROM model_calls WHERE platform_experiment_id = 'pe-1' GROUP BY agent_id, provider, model") {
		t.Errorf("query = %s", queries[0])
	}
	want := []AgentModelUsage{
		{AgentID: "agent-a", Provider: "anthropic", Model: "claude-haiku-5-5", Calls: 2, InputTokens: 15, OutputTokens: 7, CacheCreationInputTokens: 100, CacheReadInputTokens: 2000, CostUSD: 0.25},
		{AgentID: "coordinator", Provider: "openrouter", Model: "moonshotai/kimi-k2", Calls: 1, InputTokens: 1, OutputTokens: 2, CacheCreationInputTokens: 3, CacheReadInputTokens: 4, CostUSD: 0.0000504},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}

}

var anthropic = domain.ModelCatalog{"claude-haiku-5-5": {InputUSDPerMTok: 1, OutputUSDPerMTok: 5, CacheWriteUSDPerMTok: 2, CacheReadUSDPerMTok: 0.1}}

// oldStore is a store from before model_calls: model_usage made before calls had a cost, holding
// one call recorded so, and one recorded with its cost once cost_usd was added to it.
func oldStore(t *testing.T, model string) *Store {
	t.Helper()
	s := emptyStore(t)
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE model_usage ("org_id" STRING NULL, "platform_experiment_id" STRING NULL, "agent_id" STRING NULL, "provider" STRING NULL, "model" STRING NULL,
	"input_tokens" BIGINT NULL, "output_tokens" BIGINT NULL, "cache_creation_input_tokens" BIGINT NULL, "cache_read_input_tokens" BIGINT NULL, "ts" TIMESTAMP(3) NOT NULL,
	TIME INDEX ("ts"), PRIMARY KEY ("org_id", "platform_experiment_id", "agent_id", "provider", "model")) WITH (append_mode = 'true')`,
		`INSERT INTO model_usage (org_id, platform_experiment_id, agent_id, provider, model, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens, ts)
VALUES ('org-1', 'pe-1', 'agent-a', 'anthropic', ` + sqlQuote(model) + `, 1000000, 200000, 500000, 10000000, 1700000000000)`,
		`ALTER TABLE model_usage ADD COLUMN "cost_usd" DOUBLE NULL`,
		`INSERT INTO model_usage (org_id, platform_experiment_id, agent_id, provider, model, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens, cost_usd, ts)
VALUES ('org-1', 'pe-1', 'agent-a', 'openrouter', 'moonshotai/kimi-k2', 8, 125, 0, 0, 0.5, 1700000000001)`,
	} {
		if err := s.execSQL(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// The calls of a store from before model_calls move into it, each at the cost recorded with it or,
// recorded with none, at the anthropic catalog's price, and model_usage is gone. A second start
// moves nothing again.
func TestTheCallsOfAStoreFromBeforeModelCallsMoveIntoItPriced(t *testing.T) {
	s := oldStore(t, "claude-haiku-5-5")
	ctx := context.Background()
	for range 2 {
		if err := s.EnsureSchema(ctx, anthropic); err != nil {
			t.Fatalf("a store from before model_calls: %v", err)
		}
		usage, err := s.ModelUsageOf(ctx, "pe-1")
		if err != nil || len(usage) != 2 {
			t.Fatalf("usage = %+v, %v, want both calls", usage, err)
		}
		if u := usage[0]; u.Model != "claude-haiku-5-5" || u.Calls != 1 || math.Abs(u.CostUSD-(1+1+1+1)) > 1e-9 {
			t.Errorf("the call recorded with no cost: %+v, want it at the catalog's $4", u)
		}
		if u := usage[1]; u.Model != "moonshotai/kimi-k2" || u.Calls != 1 || u.CostUSD != 0.5 {
			t.Errorf("the call recorded with its cost: %+v, want its $0.50", u)
		}
	}
	if rows, err := s.querySQLRows(ctx, `SELECT table_name FROM information_schema.tables WHERE table_name = 'model_usage'`); err != nil || len(rows) != 0 {
		t.Fatalf("model_usage: %v %v, want it dropped", rows, err)
	}
}

// A move cut short, model_calls holding some of the calls and model_usage all of them, is made
// again whole at the next start: no call counts twice.
func TestAMoveCutShortIsMadeAgainWhole(t *testing.T) {
	s := oldStore(t, "claude-haiku-5-5")
	ctx := context.Background()
	if err := s.execSQL(ctx, modelCalls.create); err != nil {
		t.Fatal(err)
	}
	if err := s.execSQL(ctx, `INSERT INTO model_calls (org_id, platform_experiment_id, agent_id, provider, model, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens, cost_usd, ts)
SELECT org_id, platform_experiment_id, agent_id, provider, model, input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens, 0.5, ts FROM model_usage WHERE provider = 'openrouter'`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(ctx, anthropic); err != nil {
		t.Fatal(err)
	}
	usage, err := s.ModelUsageOf(ctx, "pe-1")
	if err != nil || len(usage) != 2 || usage[0].Calls != 1 || usage[1].Calls != 1 {
		t.Fatalf("usage = %+v, %v, want each call once", usage, err)
	}
}

// A call recorded with no cost of a model the catalog does not price is refused at the start,
// naming the model, and the store is left as it was.
func TestACallOfAModelTheCatalogDoesNotPriceIsRefusedAtTheStart(t *testing.T) {
	s := oldStore(t, "claude-fable-5-1")
	ctx := context.Background()
	if err := s.EnsureSchema(ctx, anthropic); err == nil || !strings.Contains(err.Error(), "claude-fable-5-1") || !strings.Contains(err.Error(), "models.anthropic.catalog") {
		t.Fatalf("got = %v, want a refusal naming the model and the catalog", err)
	}
	if rows, err := s.querySQLRows(ctx, `SELECT COUNT(*) FROM model_usage`); err != nil || len(rows) != 1 || rows[0][0] != float64(2) {
		t.Fatalf("model_usage: %v %v, want its two calls kept", rows, err)
	}
}
