package modelproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/scope"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/httpapi/middleware"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

const (
	signingSecret = "a-signing-secret-only-this-test-knows"
	credential    = "sk-ant-oat01-the-platforms-own-credential"
	openRouterKey = "sk-or-v1-the-platforms-own-key"
)

// directory is the platform's rows as the authenticator reads them: platform experiment pe-1 of
// org-1 and pe-2 of org-2, each running, and the agents signed up to each.
type directory struct {
	auth.Directory
	signedUp map[string]bool
	cutAt    map[string]time.Time
}

func (d directory) PlatformExperimentTenancy(_ context.Context, id string) (*domain.Tenancy, *time.Time, error) {
	org := map[string]string{"pe-1": "org-1", "pe-2": "org-2"}[id]
	if org == "" {
		return nil, nil, nil
	}
	return &domain.Tenancy{PlatformExperimentID: id, ProjectID: "project-" + org, OrgID: org}, nil, nil
}

func (d directory) IsSignedUp(_ context.Context, pe, agent string) (bool, error) {
	return d.signedUp[pe+"/"+agent], nil
}

func (d directory) AgentCutAt(_ context.Context, pe, agent string) (*time.Time, error) {
	if at, ok := d.cutAt[pe+"/"+agent]; ok {
		return &at, nil
	}
	return nil, nil
}

func (directory) AgentFinishedAt(context.Context, string, string) (*time.Time, error) {
	return nil, nil
}
func (directory) PlannerAnsweredAt(context.Context, string, string) (*time.Time, error) {
	return nil, nil
}
func (directory) GetCluster(_ context.Context, name string) (*domain.Cluster, error) {
	return &domain.Cluster{Name: name, OrgID: "org-1"}, nil
}

// sandboxed is the model choice of each agent and coordinator the platform runs, by
// <platform experiment>/<agent id>, the coordinator's agent id empty.
type sandboxed map[string]domain.ModelChoice

func (h sandboxed) ProxiedModel(_ context.Context, pe, agent string) (*domain.ModelChoice, error) {
	if choice, ok := h[pe+"/"+agent]; ok {
		return &choice, nil
	}
	return nil, nil
}

// defaults is the platform's models of an agent and a coordinator that name none.
var defaults = domain.ModelDefaults{Agent: "anthropic/claude-haiku-5-5", Coordinator: "anthropic/claude-opus-5-5"}

// runs is whom the platform runs: agent-a on the platform's default, every other on its own model,
// agent-or on an OpenRouter model and agent-oai on an openai one.
var runs = sandboxed{
	"pe-1/agent-a":      {Role: domain.AgentRoleExecutor},
	"pe-1/":             {Role: domain.AgentRoleCoordinator},
	"pe-1/agent-cut":    {Role: domain.AgentRoleExecutor},
	"pe-1/agent-ending": {Role: domain.AgentRoleExecutor},
	"pe-1/agent-or":     {Role: domain.AgentRoleExecutor, Own: "openrouter/moonshotai/kimi-k2"},
	"pe-1/agent-oai":    {Role: domain.AgentRoleResearcher, Coordinator: "openai/gpt-6-luna"},
	"pe-2/agent-b":      {Role: domain.AgentRoleExecutor, Type: "anthropic/claude-opus-5-5"},
}

// budgets is each platform experiment's model budget, pe-1 and pe-2 with room left unless set.
type budgets map[string]domain.ModelBudget

func (b budgets) ModelBudget(_ context.Context, pe string) (domain.ModelBudget, error) {
	if budget, ok := b[pe]; ok {
		return budget, nil
	}
	return domain.ModelBudget{BudgetUSD: 50, UsedUSD: 1}, nil
}

var catalog = domain.ModelCatalog{
	"claude-haiku-5-5": {InputUSDPerMTok: 0.1, OutputUSDPerMTok: 0.5, CacheWriteUSDPerMTok: 0.2, CacheReadUSDPerMTok: 0.01},
	"claude-opus-5-5":  {InputUSDPerMTok: 4, OutputUSDPerMTok: 20, CacheWriteUSDPerMTok: 8, CacheReadUSDPerMTok: 0.2},
}

// both is Anthropic at upstream and OpenRouter at upstream's /api, as openrouter.URL is.
func both(upstream string) Providers {
	return Providers{
		Anthropic:  &Anthropic{URL: upstream, OAuthToken: credential, Models: catalog},
		OpenRouter: &OpenRouter{URL: upstream + "/api", APIKey: openRouterKey},
	}
}

type recorded struct {
	mu    sync.Mutex
	calls []metricsdb.ModelCall
	done  chan struct{}
}

func (r *recorded) RecordModelCall(_ context.Context, call metricsdb.ModelCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
	close(r.done)
	return nil
}

// wait is the one call recorded, waited for: it is written once the answer has been sent.
func (r *recorded) wait(t *testing.T) metricsdb.ModelCall {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("no usage was recorded")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(r.calls))
	}
	return r.calls[0]
}

func (r *recorded) none(t *testing.T) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 0 {
		t.Fatalf("recorded %+v, want nothing", r.calls)
	}
}

// fixture is the API's front (the logging middleware and auth.Gate, as control-service mounts
// them) with the proxy behind it, and a provider the proxy reaches.
type fixture struct {
	api      *httptest.Server
	upstream string
	usage    *recorded
	mu       sync.Mutex
	received []*http.Request
	bodies   []string
}

func newFixture(t *testing.T, provider http.HandlerFunc) *fixture {
	t.Helper()
	f := &fixture{usage: &recorded{done: make(chan struct{})}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.received = append(f.received, r.Clone(context.Background()))
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()
		provider(w, r)
	}))
	t.Cleanup(upstream.Close)
	f.upstream = upstream.URL
	f.api = newAPI(t, both(upstream.URL), f.usage, budgets{}, 8)
	return f
}

func newAPI(t *testing.T, providers Providers, usage Usage, spent budgets, maxInFlightPerOrg int) *httptest.Server {
	t.Helper()
	authenticator, err := auth.NewAuthenticator(config.AuthConfig{}, signingSecret, directory{
		signedUp: map[string]bool{"pe-1/agent-a": true, "pe-1/agent-self": true, "pe-1/agent-cut": true, "pe-1/agent-ending": true, "pe-1/agent-or": true, "pe-1/agent-oai": true, "pe-2/agent-b": true},
		cutAt:    map[string]time.Time{"pe-1/agent-cut": time.Now().Add(-domain.AgentWindDown - time.Minute), "pe-1/agent-ending": time.Now().Add(-30 * time.Second)},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := New(runs, usage, spent, providers, defaults, maxInFlightPerOrg, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(middleware.Logging(zap.NewNop()))
	r.Use(auth.Gate(authenticator, zap.NewNop(), nil))
	r.Handle(Prefix+"*", proxy)
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	return server
}

func token(t *testing.T, scope string) string {
	t.Helper()
	minted, err := auth.Mint(signingSecret, scope)
	if err != nil {
		t.Fatal(err)
	}
	return minted
}

// call is a model call as Claude Code makes it with ANTHROPIC_AUTH_TOKEN set to the agent's token.
func (f *fixture) call(t *testing.T, ctx context.Context, path, bearer string) *http.Response {
	t.Helper()
	return callOn(t, ctx, f.api.URL, path, bearer, "claude-haiku-5-5")
}

func callOn(t *testing.T, ctx context.Context, api, path, bearer, model string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+path, strings.NewReader(`{"model":"`+model+`","max_tokens":16,"stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219, oauth-2025-04-20,effort-2025-11-24")
	req.Header.Set("User-Agent", "claude-cli/2.1.294 (external, sdk-cli)")
	req.Header.Set("X-Claude-Code-Session-Id", "session-1")
	req.Header.Set("X-Api-Key", bearer)
	req.Header.Set("Cookie", "session="+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (f *fixture) upstreamCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

const (
	messagesCall           = Prefix + "anthropic/v1/messages?beta=true"
	openRouterMessagesCall = Prefix + "openrouter/v1/messages?beta=true"
)

func streamed(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		for _, event := range events {
			_, _ = io.WriteString(w, event)
			w.(http.Flusher).Flush()
		}
	}
}

const (
	messageStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-haiku-5-5\",\"content\":[],\"usage\":{\"input_tokens\":12,\"cache_creation_input_tokens\":300,\"cache_read_input_tokens\":4000,\"output_tokens\":1}}}\n\n"
	textDelta    = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	messageDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":42}}\n\n"
	messageStop  = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

func TestACallWithoutATokenIsRefusedBeforeTheProvider(t *testing.T) {
	f := newFixture(t, streamed(messageStart))
	resp := f.call(t, context.Background(), messagesCall, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || f.upstreamCalls() != 0 {
		t.Fatalf("status = %d after %d provider calls, want 401 and none", resp.StatusCode, f.upstreamCalls())
	}
}

// Only the agents and coordinators the platform runs make model calls through it, while their
// token lives: an agent's last turn in its wind-down still runs, and nothing after it does.
func TestOnlyASandboxedAgentOrCoordinatorWhoseTokenLivesIsServed(t *testing.T) {
	for _, tc := range []struct {
		name, scope string
		want        int
	}{
		{"a sandboxed agent", scope.Agent("pe-1", "agent-a"), http.StatusOK},
		{"a sandboxed coordinator", scope.Coordinator("pe-1"), http.StatusOK},
		{"an agent in its wind-down, for its last turn", scope.Agent("pe-1", "agent-ending"), http.StatusOK},
		{"an agent the platform does not run", scope.Agent("pe-1", "agent-self"), http.StatusForbidden},
		{"an agent whose wind-down is over", scope.Agent("pe-1", "agent-cut"), http.StatusUnauthorized},
		{"an agent of another platform experiment than its token's", scope.Agent("pe-1", "agent-b"), http.StatusUnauthorized},
		{"a coordinator the platform does not run", scope.Coordinator("pe-2"), http.StatusForbidden},
		{"a cluster's runtime", scope.Runtime("cluster-1", time.Time{}), http.StatusForbidden},
	} {
		f := newFixture(t, streamed(messageStart, messageStop))
		resp := f.call(t, context.Background(), messagesCall, token(t, tc.scope))
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		wantCalls := 0
		if tc.want == http.StatusOK {
			wantCalls = 1
		}
		if resp.StatusCode != tc.want || f.upstreamCalls() != wantCalls {
			t.Errorf("%s: status = %d after %d provider calls, want %d after %d", tc.name, resp.StatusCode, f.upstreamCalls(), tc.want, wantCalls)
		}
	}
}

func TestOnlyTheProvidersMessageCallsAreServed(t *testing.T) {
	f := newFixture(t, streamed(messageStart, messageStop))
	agent := token(t, scope.Agent("pe-1", "agent-a"))
	for _, path := range []string{
		Prefix + "anthropic/v1/models",
		Prefix + "anthropic/api/oauth/usage",
		Prefix + "anthropic/v1/messages/batches",
		Prefix + "openrouter/v1/models",
		Prefix + "openai/v1/messages",
	} {
		resp := f.call(t, context.Background(), path, agent)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
	if f.upstreamCalls() != 0 {
		t.Fatalf("the provider was called %d times", f.upstreamCalls())
	}
}

// The provider gets the platform's credential in Claude Code's own OAuth shape, and nothing that
// carries the agent's platform token.
func TestTheCallCarriesThePlatformCredentialAndNeverThePlatformToken(t *testing.T) {
	f := newFixture(t, streamed(messageStart, messageStop))
	agent := token(t, scope.Agent("pe-1", "agent-a"))
	resp := f.call(t, context.Background(), messagesCall, agent)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if f.upstreamCalls() != 1 {
		t.Fatalf("provider calls = %d", f.upstreamCalls())
	}
	got := f.received[0]
	if got.URL.Path != "/v1/messages" || got.URL.RawQuery != "beta=true" || got.Method != http.MethodPost {
		t.Errorf("provider got %s %s?%s", got.Method, got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Bearer "+credential {
		t.Errorf("Authorization does not carry the platform's credential")
	}
	if beta := got.Header.Values("Anthropic-Beta"); len(beta) != 1 || beta[0] != "oauth-2025-04-20,claude-code-20250219,effort-2025-11-24" {
		t.Errorf("anthropic-beta = %q, want the OAuth beta first and the agent's once each", beta)
	}
	for name, want := range map[string]string{"Anthropic-Version": "2023-06-01", "User-Agent": "claude-cli/2.1.294 (external, sdk-cli)", "X-Claude-Code-Session-Id": "session-1", "Content-Type": "application/json"} {
		if got.Header.Get(name) != want {
			t.Errorf("%s = %q, want %q passed on", name, got.Header.Get(name), want)
		}
	}
	for _, name := range []string{"X-Api-Key", "Cookie", "X-Forwarded-For", "Forwarded"} {
		if got.Header.Get(name) != "" {
			t.Errorf("%s was passed on", name)
		}
	}
	for name, values := range got.Header {
		for _, value := range values {
			if strings.Contains(value, agent) {
				t.Errorf("header %s carries the platform token", name)
			}
		}
	}
	if !strings.Contains(f.bodies[0], `"model":"claude-haiku-5-5"`) {
		t.Errorf("body = %s, want the agent's passed on", f.bodies[0])
	}
}

// The harness paces the account's allowance from the provider's rate-limit headers, and waits out
// a refusal by its reset: both reach it as the provider sent them.
func TestTheProvidersRateLimitReachesTheAgentAsItWasSent(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", "1760000000")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1.02")
		w.Header().Set("Retry-After", "1800")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`)
	})
	resp := f.call(t, context.Background(), messagesCall, token(t, scope.Agent("pe-1", "agent-a")))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "rate_limit_error") {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	for name, want := range map[string]string{"Anthropic-Ratelimit-Unified-Status": "rejected", "Anthropic-Ratelimit-Unified-Reset": "1760000000", "Anthropic-Ratelimit-Unified-5h-Utilization": "1.02", "Retry-After": "1800"} {
		if resp.Header.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, resp.Header.Get(name), want)
		}
	}
	f.usage.none(t)
}

// Each event reaches the agent while the provider is still writing the next: the proxy holds back
// nothing of a stream. Its usage is recorded once it ends.
func TestAStreamedAnswerPassesEventByEventAndItsUsageIsRecorded(t *testing.T) {
	read := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, messageStart)
		w.(http.Flusher).Flush()
		select {
		case <-read:
		case <-time.After(5 * time.Second):
			return
		}
		for _, event := range []string{textDelta, messageDelta, messageStop} {
			_, _ = io.WriteString(w, event)
			w.(http.Flusher).Flush()
		}
	})
	resp := f.call(t, context.Background(), messagesCall, token(t, scope.Agent("pe-1", "agent-a")))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" {
		t.Fatalf("status = %d, headers %v", resp.StatusCode, resp.Header)
	}
	lines := bufio.NewReader(resp.Body)
	first, err := lines.ReadString('\n')
	if err != nil || first != "event: message_start\n" {
		t.Fatalf("first line = %q, %v: the stream's first event did not arrive before the provider wrote the rest", first, err)
	}
	close(read)
	rest, _ := io.ReadAll(lines)
	if !strings.HasSuffix(string(rest), messageStop) {
		t.Fatalf("rest = %q", rest)
	}
	got := f.usage.wait(t)
	want := metricsdb.ModelCall{OrgID: "org-1", PlatformExperimentID: "pe-1", AgentID: "agent-a", Provider: "anthropic", Model: "claude-haiku-5-5",
		InputTokens: 12, OutputTokens: 42, CacheCreationInputTokens: 300, CacheReadInputTokens: 4000, CostUSD: got.CostUSD, At: got.At}
	if got != want || got.At.IsZero() || math.Abs(got.CostUSD-(12*0.1+42*0.5+300*0.2+4000*0.01)/1e6) > 1e-12 {
		t.Fatalf("recorded %+v, want %+v", got, want)
	}
}

// An agent that hangs up ends the provider's call, and what the provider counted before is still
// recorded: the input was spent.
func TestAnAgentThatHangsUpEndsTheProvidersCall(t *testing.T) {
	ended := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, messageStart)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(ended)
		case <-time.After(5 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	resp := f.call(t, ctx, messagesCall, token(t, scope.Coordinator("pe-1")))
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the provider's call went on after the agent hung up")
	}
	got := f.usage.wait(t)
	if got.AgentID != domain.CoordinatorRecipient || got.InputTokens != 12 || got.OutputTokens != 1 || got.CacheReadInputTokens != 4000 {
		t.Fatalf("recorded %+v", got)
	}
}

// OpenRouter states a call's usage and cost only at the end of its answer: an agent that hangs up
// leaves the call to run to its end, and its whole usage and cost are recorded.
func TestAnOpenRouterCallTheAgentHungUpOnIsReadToItsEndForItsCost(t *testing.T) {
	hungUp := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, openRouterStart)
		w.(http.Flusher).Flush()
		<-hungUp
		time.Sleep(100 * time.Millisecond)
		for _, event := range []string{textDelta, openRouterDelta, messageStop} {
			_, _ = io.WriteString(w, event)
			w.(http.Flusher).Flush()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	resp := callOn(t, ctx, f.api.URL, openRouterMessagesCall, token(t, scope.Agent("pe-1", "agent-or")), "moonshotai/kimi-k2")
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close()
	close(hungUp)
	if got := f.usage.wait(t); got.AgentID != "agent-or" || got.InputTokens != 8 || got.OutputTokens != 125 || got.CostUSD != 0.0000504 {
		t.Fatalf("recorded %+v, want the call's final usage and cost", got)
	}
}

func TestAnAnswerThatIsNotStreamedIsMeteredWhole(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-haiku-5-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":7}}`)
	})
	resp := f.call(t, context.Background(), messagesCall, token(t, scope.Agent("pe-2", "agent-b")))
	var message map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&message); err != nil || message["id"] != "msg_1" {
		t.Fatalf("answer = %v, %v", message, err)
	}
	resp.Body.Close()
	got := f.usage.wait(t)
	if got.OrgID != "org-2" || got.PlatformExperimentID != "pe-2" || got.AgentID != "agent-b" || got.InputTokens != 5 || got.OutputTokens != 3 || got.CacheReadInputTokens != 7 {
		t.Fatalf("recorded %+v", got)
	}
}

func TestCountingTokensSpendsNoneAndIsNotRecorded(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":99}`)
	})
	resp := f.call(t, context.Background(), Prefix+"anthropic/v1/messages/count_tokens?beta=true", token(t, scope.Agent("pe-1", "agent-a")))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"input_tokens":99}` {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	f.usage.none(t)
}

// The API server's deadlines are sized for API calls: a model's answer streams for longer, and
// reaches the agent whole.
func TestAnAnswerOutlastsTheServersOwnDeadlines(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, event := range []string{messageStart, textDelta, textDelta, messageDelta, messageStop} {
			_, _ = io.WriteString(w, event)
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
		}
	}))
	defer upstream.Close()
	front := newAPI(t, both(upstream.URL), &recorded{done: make(chan struct{})}, budgets{}, 8)
	front.Close()
	slow := httptest.NewUnstartedServer(front.Config.Handler)
	slow.Config.ReadTimeout, slow.Config.WriteTimeout = 100*time.Millisecond, 100*time.Millisecond
	slow.Start()
	defer slow.Close()
	req, _ := http.NewRequest(http.MethodPost, slow.URL+messagesCall, strings.NewReader(`{"model":"claude-opus-5-5"}`))
	req.Header.Set("Authorization", "Bearer "+token(t, scope.Agent("pe-1", "agent-a")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !strings.HasSuffix(string(body), messageStop) {
		t.Fatalf("got %q, %v: the answer was cut at the server's deadline", body, err)
	}
}

func TestAProviderThatCannotBeReachedIsAnsweredInItsOwnErrorShape(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	server := newAPI(t, both(gone.URL), &recorded{done: make(chan struct{})}, budgets{}, 8)
	req, _ := http.NewRequest(http.MethodPost, server.URL+messagesCall, strings.NewReader(`{"model":"claude-opus-5-5"}`))
	req.Header.Set("Authorization", "Bearer "+token(t, scope.Agent("pe-1", "agent-a")))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), `"type":"api_error"`) {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func (d directory) AgentStanding(ctx context.Context, pe, agent string) (domain.AgentStanding, error) {
	tenancy, closedAt, err := d.PlatformExperimentTenancy(ctx, pe)
	if err != nil || tenancy == nil {
		return domain.AgentStanding{}, err
	}
	signedUp, err := d.IsSignedUp(ctx, pe, agent)
	if err != nil {
		return domain.AgentStanding{}, err
	}
	cutAt, err := d.AgentCutAt(ctx, pe, agent)
	if err != nil {
		return domain.AgentStanding{}, err
	}
	answeredAt, err := d.PlannerAnsweredAt(ctx, pe, agent)
	if err != nil {
		return domain.AgentStanding{}, err
	}
	finishedAt, err := d.AgentFinishedAt(ctx, pe, agent)
	if err != nil {
		return domain.AgentStanding{}, err
	}
	return domain.AgentStanding{Tenancy: tenancy, ClosedAt: closedAt, SignedUp: signedUp, CutAt: cutAt, PlannerAnsweredAt: answeredAt, FinishedAt: finishedAt}, nil
}

// refused reads a refusal: its status, reason and Retry-After.
func refused(t *testing.T, resp *http.Response) (int, string, string) {
	t.Helper()
	defer resp.Body.Close()
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body.Reason, resp.Header.Get("Retry-After")
}

// Claude Code's subagents and small tasks call the models of the catalog; any other model is
// refused before the provider, counting tokens too.
func TestAModelOutsideTheCatalogIsRefusedBeforeTheProvider(t *testing.T) {
	f := newFixture(t, streamed(messageStart, messageStop))
	agent := token(t, scope.Agent("pe-1", "agent-a"))
	resp := callOn(t, context.Background(), f.api.URL, messagesCall, agent, "claude-opus-5-5")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a catalog model: status %d", resp.StatusCode)
	}
	for _, path := range []string{messagesCall, Prefix + "anthropic/v1/messages/count_tokens"} {
		for _, model := range []string{"claude-fable-5-1", ""} {
			if status, reason, _ := refused(t, callOn(t, context.Background(), f.api.URL, path, agent, model)); status != http.StatusForbidden || reason != domain.ReasonModelNotServed {
				t.Errorf("%s with model %q: %d %s, want 403 %s", path, model, status, reason, domain.ReasonModelNotServed)
			}
		}
	}
	if f.upstreamCalls() != 1 {
		t.Fatalf("the provider was called %d times, want once", f.upstreamCalls())
	}
}

// Once a platform experiment's model calls cost its budget, its agents and coordinator are
// refused every call, and another platform experiment's go on.
func TestAPlatformExperimentWhoseModelBudgetIsSpentIsRefused(t *testing.T) {
	var calls int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		streamed(messageStart, messageStop)(w, r)
	}))
	t.Cleanup(upstream.Close)
	api := newAPI(t, both(upstream.URL), &recorded{done: make(chan struct{})}, budgets{"pe-1": {BudgetUSD: 50, UsedUSD: 50.01}}, 8)
	for _, c := range []struct{ bearer, path, model string }{
		{token(t, scope.Agent("pe-1", "agent-a")), messagesCall, "claude-haiku-5-5"},
		{token(t, scope.Coordinator("pe-1")), messagesCall, "claude-opus-5-5"},
		{token(t, scope.Agent("pe-1", "agent-or")), openRouterMessagesCall, "moonshotai/kimi-k2"},
	} {
		status, reason, retry := refused(t, callOn(t, context.Background(), api.URL, c.path, c.bearer, c.model))
		if status != http.StatusTooManyRequests || reason != domain.ReasonModelBudgetSpent || retry != "60" {
			t.Errorf("%s: got %d %s Retry-After %q, want 429 %s after 60", c.path, status, reason, retry, domain.ReasonModelBudgetSpent)
		}
	}
	resp := callOn(t, context.Background(), api.URL, messagesCall, token(t, scope.Agent("pe-2", "agent-b")), "claude-haiku-5-5")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if resp.StatusCode != http.StatusOK || calls != 1 {
		t.Fatalf("another platform experiment: status %d after %d provider calls, want 200 after 1", resp.StatusCode, calls)
	}
}

// An org has at most MaxInFlightPerOrg calls in flight; another org's calls are not held to its,
// and a call that ends frees its place.
func TestAnOrgHasAtMostItsCallsInFlight(t *testing.T) {
	answer := make(chan struct{})
	started := make(chan struct{}, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-answer
		streamed(messageStart, messageStop)(w, r)
	}))
	t.Cleanup(upstream.Close)
	api := newAPI(t, both(upstream.URL), &recorded{done: make(chan struct{})}, budgets{}, 2)
	agent := token(t, scope.Agent("pe-1", "agent-a"))
	results := make(chan int, 2)
	for range 2 {
		go func() {
			resp := callOn(t, context.Background(), api.URL, messagesCall, agent, "claude-haiku-5-5")
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			results <- resp.StatusCode
		}()
		<-started
	}
	status, reason, retry := refused(t, callOn(t, context.Background(), api.URL, messagesCall, token(t, scope.Coordinator("pe-1")), "claude-haiku-5-5"))
	if status != http.StatusTooManyRequests || reason != domain.ReasonModelCallsInFlight || retry != "60" {
		t.Fatalf("a third call of org-1: %d %s Retry-After %q, want 429 %s", status, reason, retry, domain.ReasonModelCallsInFlight)
	}
	other := make(chan int, 1)
	go func() {
		resp := callOn(t, context.Background(), api.URL, messagesCall, token(t, scope.Agent("pe-2", "agent-b")), "claude-haiku-5-5")
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		other <- resp.StatusCode
	}()
	<-started
	close(answer)
	for range 2 {
		if got := <-results; got != http.StatusOK {
			t.Fatalf("a call in flight: status %d", got)
		}
	}
	if got := <-other; got != http.StatusOK {
		t.Fatalf("org-2's call: status %d", got)
	}
	resp := callOn(t, context.Background(), api.URL, messagesCall, agent, "claude-haiku-5-5")
	<-started
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a call after the others ended: status %d", resp.StatusCode)
	}
}

const (
	openRouterStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"moonshotai/kimi-k2\",\"content\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"cache_creation_input_tokens\":null,\"cache_read_input_tokens\":null}}}\n\n"
	openRouterDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":8,\"output_tokens\":125,\"cache_creation_input_tokens\":null,\"cache_read_input_tokens\":0,\"cost\":0.0000504,\"is_byok\":false}}\n\n"
)

// An OpenRouter call goes to OpenRouter's Anthropic-compatible API with the platform's key and
// nothing of the caller's, not even the Anthropic OAuth beta, and is recorded at the cost
// OpenRouter's answer states.
func TestAnOpenRouterCallCarriesThePlatformsKeyAndIsRecordedAtItsStatedCost(t *testing.T) {
	f := newFixture(t, streamed(openRouterStart, textDelta, openRouterDelta, messageStop))
	agent := token(t, scope.Agent("pe-1", "agent-or"))
	resp := callOn(t, context.Background(), f.api.URL, openRouterMessagesCall, agent, "moonshotai/kimi-k2")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasSuffix(string(body), messageStop) {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	got := f.received[0]
	if got.URL.Path != "/api/v1/messages" || got.URL.RawQuery != "beta=true" {
		t.Errorf("OpenRouter got %s?%s, want /api/v1/messages", got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Bearer "+openRouterKey {
		t.Errorf("Authorization does not carry the platform's OpenRouter key")
	}
	if beta := got.Header.Values("Anthropic-Beta"); len(beta) != 1 || beta[0] != "claude-code-20250219,effort-2025-11-24" {
		t.Errorf("anthropic-beta = %q, want the agent's without the OAuth beta", beta)
	}
	for name, values := range got.Header {
		for _, value := range values {
			if strings.Contains(value, agent) || strings.Contains(value, credential) {
				t.Errorf("header %s carries the platform token or the Anthropic credential", name)
			}
		}
	}
	call := f.usage.wait(t)
	want := metricsdb.ModelCall{OrgID: "org-1", PlatformExperimentID: "pe-1", AgentID: "agent-or", Provider: "openrouter", Model: "moonshotai/kimi-k2",
		InputTokens: 8, OutputTokens: 125, CostUSD: 0.0000504, At: call.At}
	if call != want {
		t.Fatalf("recorded %+v, want %+v", call, want)
	}
}

// The harness sets every model Claude Code calls to the agent's own, so a call to OpenRouter names
// that model and no other.
func TestAnOpenRouterCallNamesItsCallersModelAndNoOther(t *testing.T) {
	f := newFixture(t, streamed(openRouterStart, openRouterDelta, messageStop))
	agent := token(t, scope.Agent("pe-1", "agent-or"))
	for _, model := range []string{"", "openai/gpt-5-nano"} {
		if status, reason, _ := refused(t, callOn(t, context.Background(), f.api.URL, openRouterMessagesCall, agent, model)); status != http.StatusForbidden || reason != domain.ReasonModelNotTheCallers {
			t.Errorf("model %q: %d %s, want 403 %s", model, status, reason, domain.ReasonModelNotTheCallers)
		}
	}
	resp := callOn(t, context.Background(), f.api.URL, openRouterMessagesCall, agent, "moonshotai/kimi-k2")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || f.upstreamCalls() != 1 {
		t.Fatalf("its own model: status %d after %d calls, want 200 after 1", resp.StatusCode, f.upstreamCalls())
	}
	if got := f.usage.wait(t); got.Model != "moonshotai/kimi-k2" || got.CostUSD != 0.0000504 {
		t.Fatalf("recorded %+v", got)
	}
}

// A call goes to the provider of its caller's model, as its instructions resolve it, and to no
// other: the platform pays for the model it chose for the caller.
func TestACallGoesToTheProviderOfItsCallersModelAlone(t *testing.T) {
	upstream := httptest.NewServer(streamed(messageStart, messageStop))
	t.Cleanup(upstream.Close)
	providers := both(upstream.URL)
	providers.OpenAI = &OpenAI{URL: upstream.URL, Credentials: chatGPTCredentials{}, Models: openAICatalog}
	api := newAPI(t, providers, &recorded{done: make(chan struct{})}, budgets{}, 8)
	for _, c := range []struct{ name, agent, path, model string }{
		{"an agent on the default anthropic model to openrouter", "agent-a", openRouterMessagesCall, "moonshotai/kimi-k2"},
		{"an agent of a type on an anthropic model to openai", "agent-b", Prefix + "openai/v1/messages", "gpt-6-luna"},
		{"an agent on an openrouter model to anthropic", "agent-or", messagesCall, "claude-haiku-5-5"},
		{"a researcher on the coordinator's openai model to anthropic", "agent-oai", messagesCall, "claude-haiku-5-5"},
	} {
		pe := "pe-1"
		if c.agent == "agent-b" {
			pe = "pe-2"
		}
		if status, reason, _ := refused(t, callOn(t, context.Background(), api.URL, c.path, token(t, scope.Agent(pe, c.agent)), c.model)); status != http.StatusForbidden || reason != domain.ReasonModelNotTheCallers {
			t.Errorf("%s: %d %s, want 403 %s", c.name, status, reason, domain.ReasonModelNotTheCallers)
		}
	}
}

// An answer that states no cost was still made: its tokens are recorded, at no cost.
func TestAnOpenRouterAnswerStatingNoCostIsRecordedAtNone(t *testing.T) {
	f := newFixture(t, streamed(openRouterStart, messageDelta, messageStop))
	resp := callOn(t, context.Background(), f.api.URL, openRouterMessagesCall, token(t, scope.Agent("pe-1", "agent-or")), "moonshotai/kimi-k2")
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := f.usage.wait(t); got.Provider != "openrouter" || got.OutputTokens != 42 || got.CostUSD != 0 {
		t.Fatalf("recorded %+v", got)
	}
}

// A provider whose credential the platform does not hold is not served: its calls are not found.
func TestAProviderThePlatformDoesNotServeIsNotFound(t *testing.T) {
	upstream := httptest.NewServer(streamed(messageStart, messageStop))
	t.Cleanup(upstream.Close)
	agent := token(t, scope.Agent("pe-1", "agent-a"))
	anthropicOnly := newAPI(t, Providers{Anthropic: both(upstream.URL).Anthropic}, &recorded{done: make(chan struct{})}, budgets{}, 8)
	resp := callOn(t, context.Background(), anthropicOnly.URL, openRouterMessagesCall, agent, "moonshotai/kimi-k2")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("openrouter with anthropic alone served: status %d, want 404", resp.StatusCode)
	}
	openRouterOnly := newAPI(t, Providers{OpenRouter: both(upstream.URL).OpenRouter}, &recorded{done: make(chan struct{})}, budgets{}, 8)
	resp = callOn(t, context.Background(), openRouterOnly.URL, messagesCall, agent, "claude-haiku-5-5")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("anthropic with openrouter alone served: status %d, want 404", resp.StatusCode)
	}
}
