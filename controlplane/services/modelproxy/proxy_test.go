package modelproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/config"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

const (
	signingSecret = "a-signing-secret-only-this-test-knows"
	credential    = "sk-ant-oat01-the-platforms-own-credential"
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
func (directory) ClusterOrg(context.Context, string) (string, bool, error) { return "org-1", true, nil }

type hosted map[string]bool

func (h hosted) Hosted(_ context.Context, pe, agent string) (bool, error) {
	return h[pe+"/"+agent], nil
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
	f.api = newAPI(t, upstream.URL, f.usage)
	return f
}

func newAPI(t *testing.T, upstream string, usage Usage) *httptest.Server {
	t.Helper()
	authenticator, err := auth.NewAuthenticator(config.AuthConfig{SigningSecret: signingSecret}, directory{
		signedUp: map[string]bool{"pe-1/agent-a": true, "pe-1/agent-self": true, "pe-1/agent-cut": true, "pe-1/agent-ending": true, "pe-2/agent-b": true},
		cutAt:    map[string]time.Time{"pe-1/agent-cut": time.Now().Add(-domain.AgentWindDown - time.Minute), "pe-1/agent-ending": time.Now().Add(-30 * time.Second)},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := New(hosted{"pe-1/agent-a": true, "pe-1/": true, "pe-1/agent-cut": true, "pe-1/agent-ending": true, "pe-2/agent-b": true},
		usage, upstream, credential, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Use(api.LoggingMiddleware(zap.NewNop()))
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.api.URL+path, strings.NewReader(`{"model":"claude-haiku-5-5","max_tokens":16,"stream":true,"messages":[]}`))
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

const messagesCall = Prefix + AnthropicProvider + "/v1/messages?beta=true"

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
func TestOnlyAHostedAgentOrCoordinatorWhoseTokenLivesIsServed(t *testing.T) {
	for _, tc := range []struct {
		name, scope string
		want        int
	}{
		{"a hosted agent", auth.AgentScope("pe-1", "agent-a"), http.StatusOK},
		{"a hosted coordinator", auth.CoordinatorScope("pe-1"), http.StatusOK},
		{"an agent in its wind-down, for its last turn", auth.AgentScope("pe-1", "agent-ending"), http.StatusOK},
		{"an agent the platform does not run", auth.AgentScope("pe-1", "agent-self"), http.StatusForbidden},
		{"an agent whose wind-down is over", auth.AgentScope("pe-1", "agent-cut"), http.StatusUnauthorized},
		{"an agent of another platform experiment than its token's", auth.AgentScope("pe-1", "agent-b"), http.StatusUnauthorized},
		{"a coordinator the platform does not run", auth.CoordinatorScope("pe-2"), http.StatusForbidden},
		{"a cluster's runtime", auth.RuntimeScope("cluster-1"), http.StatusForbidden},
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
	agent := token(t, auth.AgentScope("pe-1", "agent-a"))
	for _, path := range []string{
		Prefix + AnthropicProvider + "/v1/models",
		Prefix + AnthropicProvider + "/api/oauth/usage",
		Prefix + AnthropicProvider + "/v1/messages/batches",
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
	agent := token(t, auth.AgentScope("pe-1", "agent-a"))
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
	resp := f.call(t, context.Background(), messagesCall, token(t, auth.AgentScope("pe-1", "agent-a")))
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
	resp := f.call(t, context.Background(), messagesCall, token(t, auth.AgentScope("pe-1", "agent-a")))
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
		InputTokens: 12, OutputTokens: 42, CacheCreationInputTokens: 300, CacheReadInputTokens: 4000, At: got.At}
	if got != want || got.At.IsZero() {
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
	resp := f.call(t, ctx, messagesCall, token(t, auth.CoordinatorScope("pe-1")))
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

func TestAnAnswerThatIsNotStreamedIsMeteredWhole(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","model":"claude-haiku-5-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":7}}`)
	})
	resp := f.call(t, context.Background(), messagesCall, token(t, auth.AgentScope("pe-2", "agent-b")))
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
	resp := f.call(t, context.Background(), Prefix+AnthropicProvider+"/v1/messages/count_tokens?beta=true", token(t, auth.AgentScope("pe-1", "agent-a")))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"input_tokens":99}` {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	f.usage.none(t)
}

func TestAProviderThatCannotBeReachedIsAnsweredInItsOwnErrorShape(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	server := newAPI(t, gone.URL, &recorded{done: make(chan struct{})})
	req, _ := http.NewRequest(http.MethodPost, server.URL+messagesCall, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token(t, auth.AgentScope("pe-1", "agent-a")))
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
