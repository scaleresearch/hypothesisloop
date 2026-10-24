package metricsdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
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
