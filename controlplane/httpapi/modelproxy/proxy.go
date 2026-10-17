package modelproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// Prefix is where the proxy is served, the provider's name after it.
const Prefix = "/model/"

// Store says whom the platform runs. *db.Store satisfies it.
type Store interface {
	InAgentSandbox(ctx context.Context, platformExpID, agentID string) (bool, error)
}

// Usage stores what the provider counted for a call.
type Usage interface {
	RecordModelCall(ctx context.Context, call metricsdb.ModelCall) error
}

// Budgets says what a platform experiment may spend on model calls and what it spent.
// *platformexperiments.Service satisfies it.
type Budgets interface {
	ModelBudget(ctx context.Context, platformExpID string) (domain.ModelBudget, error)
}

// Policy is what the platform allows the model calls it pays for.
type Policy struct {
	// Models are the models a call may name.
	Models domain.ModelCatalog
	// MaxInFlightPerOrg is how many calls of one org are made at once.
	MaxInFlightPerOrg int
}

const (
	// AnthropicProvider is the one provider served, at Prefix + AnthropicProvider.
	AnthropicProvider = "anthropic"
	// AnthropicUpstream is where Anthropic's API is reached.
	AnthropicUpstream = "https://api.anthropic.com"
	// oauthBeta is the beta Anthropic's API takes a Claude subscription token under, which Claude
	// Code sends with one and leaves out with any other credential.
	oauthBeta = "oauth-2025-04-20"
	// callLimit is how long one call may take through the proxy. The API server's own deadlines
	// are sized for API calls, far shorter than a model's answer streams for.
	callLimit = time.Hour
	// recordLimit bounds the write of a call's usage, made after the answer is sent.
	recordLimit = 5 * time.Second
	// maxRequestBytes is the most the provider takes in one call.
	maxRequestBytes = 32 << 20
	// retryAfter is when a call refused for its org's calls in flight or its spent budget is
	// worth making again: Claude Code waits it out and retries.
	retryAfter = time.Minute
)

// messagesPath is the call that spends tokens, and the only one metered.
const messagesPath = "/v1/messages"

// served are the provider's endpoints an agent may call, by path, with their method.
var served = map[string]string{
	messagesPath:                http.MethodPost,
	"/v1/messages/count_tokens": http.MethodPost,
}

// Proxy serves Prefix + AnthropicProvider behind auth.Gate.
type Proxy struct {
	store    Store
	usage    Usage
	budgets  Budgets
	policy   Policy
	upstream *url.URL
	reverse  *httputil.ReverseProxy
	logger   *zap.Logger
	now      func() time.Time

	// inFlight counts each org's calls this process is making: live connections, which no store
	// holds.
	mu       sync.Mutex
	inFlight map[string]int
}

// New is the proxy to the Anthropic API at upstream, which makes every call with oauthToken, a
// Claude subscription token, as policy allows.
func New(store Store, usage Usage, budgets Budgets, policy Policy, upstream, oauthToken string, logger *zap.Logger) (*Proxy, error) {
	if store == nil || usage == nil || budgets == nil || logger == nil {
		return nil, errors.New("modelproxy.New: a store, a usage store, the budgets and a logger are required")
	}
	if err := policy.Models.Validate(); err != nil {
		return nil, fmt.Errorf("modelproxy.New: %w", err)
	}
	if policy.MaxInFlightPerOrg <= 0 {
		return nil, errors.New("modelproxy.New: the calls an org may have in flight are positive")
	}
	if oauthToken == "" {
		return nil, errors.New("modelproxy.New: the provider's credential is required")
	}
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return nil, errors.New("modelproxy.New: the upstream is an http(s) address")
	}
	p := &Proxy{store: store, usage: usage, budgets: budgets, policy: policy, upstream: target, logger: logger, now: time.Now, inFlight: map[string]int{}}
	p.reverse = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			withCredential(r.Out.Header, oauthToken)
		},
		// Each event of a streamed answer reaches the agent as it arrives.
		FlushInterval:  -1,
		ModifyResponse: p.meter,
		ErrorHandler:   p.failed,
		ErrorLog:       zap.NewStdLog(logger),
	}
	return p, nil
}

// withCredential makes the outgoing headers carry the platform's credential and nothing of the
// caller's: its platform token is the caller's alone and never leaves the platform. The transport
// asks for and undoes the compression itself, so the proxy reads the usage of every answer.
func withCredential(h http.Header, oauthToken string) {
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Accept-Encoding"} {
		h.Del(name)
	}
	h.Set("Authorization", "Bearer "+oauthToken)
	var betas []string
	for _, value := range h.Values("Anthropic-Beta") {
		for _, beta := range strings.Split(value, ",") {
			if beta = strings.TrimSpace(beta); beta != "" && beta != oauthBeta {
				betas = append(betas, beta)
			}
		}
	}
	h.Set("Anthropic-Beta", strings.Join(append([]string{oauthBeta}, betas...), ","))
}

// caller is whom a call is made and metered for.
type caller struct {
	org, platformExperiment, agent string
}

type callerKey struct{}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, _ := strings.CutPrefix(r.URL.Path, Prefix)
	provider, endpoint, _ := strings.Cut(rest, "/")
	endpoint = "/" + endpoint
	if provider != AnthropicProvider {
		auth.WriteError(w, p.logger, auth.NotFound("model provider "+provider+": the platform's agents run on "+AnthropicProvider+", at "+Prefix+AnthropicProvider))
		return
	}
	if method, ok := served[endpoint]; !ok || r.Method != method {
		auth.WriteError(w, p.logger, auth.NotFound(r.Method+" "+endpoint+": the model proxy serves POST "+messagesPath+" and POST /v1/messages/count_tokens"))
		return
	}
	who, err := p.authorize(r.Context())
	if err != nil {
		auth.WriteError(w, p.logger, err)
		return
	}
	release, err := p.admit(r, who)
	if err != nil {
		var refused *refusal.Refusal
		if errors.As(err, &refused) && refused.GetStatus() == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
		}
		auth.WriteError(w, p.logger, err)
		return
	}
	defer release()
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(callLimit)
	if err := errors.Join(controller.SetReadDeadline(deadline), controller.SetWriteDeadline(deadline)); err != nil {
		auth.WriteError(w, p.logger, auth.Internal(p.logger, "model proxy: set the deadlines of a call", err))
		return
	}
	out := r.Clone(context.WithValue(r.Context(), callerKey{}, who))
	out.URL.Path = endpoint
	out.URL.RawPath = ""
	p.reverse.ServeHTTP(w, out)
}

// authorize says whom the caller's call is made for: an agent or the coordinator of a platform
// experiment, while auth.Gate takes its token, and only one the platform runs. An agent run
// elsewhere brings its own model.
func (p *Proxy) authorize(ctx context.Context) (caller, error) {
	principal, ok := auth.FromContext(ctx)
	if !ok {
		return caller{}, auth.Unauthenticated("the request carries no principal")
	}
	var who caller
	switch principal.Kind {
	case auth.KindAgent:
		who = caller{platformExperiment: principal.PlatformExperimentID, agent: principal.AgentID}
	case auth.KindCoordinator:
		who = caller{platformExperiment: principal.PlatformExperimentID, agent: domain.CoordinatorRecipient}
	default:
		return caller{}, auth.Forbidden("a " + string(principal.Kind) + " token makes no model call through the platform: it makes those of the agents and coordinators it runs")
	}
	if len(principal.OrgIDs) != 1 || principal.PlatformExperimentID == "" {
		return caller{}, auth.Internal(p.logger, "model proxy: place a caller", errors.New("an agent or coordinator principal without its one org and platform experiment"))
	}
	who.org = principal.OrgIDs[0]
	sandboxed, err := p.store.InAgentSandbox(ctx, principal.PlatformExperimentID, principal.AgentID)
	if err != nil {
		return caller{}, auth.Internal(p.logger, "model proxy: read whether the platform runs a caller", err, zap.String("platform_experiment_id", principal.PlatformExperimentID))
	}
	if !sandboxed {
		return caller{}, auth.Forbidden("the platform does not run " + who.agent + " of platform experiment " + who.platformExperiment + ", so its model calls are not made through the platform")
	}
	return who, nil
}

// admit is the platform's policy on a call it pays for, applied here and nowhere else: the call
// names a model of the catalog, the platform experiment's model calls have not cost its model
// budget, and its org has a call in flight to spare. release ends the call's hold on its org's.
// The caller's token, which outlives its run by domain.AgentWindDown or domain.CoordinatorWindDown
// for its last turn, is auth.Gate's to refuse after.
func (p *Proxy) admit(r *http.Request, who caller) (release func(), err error) {
	model, err := requestedModel(r)
	if err != nil {
		return nil, err
	}
	if _, ok := p.policy.Models[model]; !ok {
		return nil, refusal.Refuse(http.StatusForbidden, domain.ReasonModelNotServed,
			fmt.Sprintf("model %q is not one the platform serves: name one of %s", model, strings.Join(p.policy.Models.Names(), ", ")))
	}
	budget, err := p.budgets.ModelBudget(r.Context(), who.platformExperiment)
	if err != nil {
		return nil, auth.Internal(p.logger, "model proxy: read a platform experiment's model budget", err, zap.String("platform_experiment_id", who.platformExperiment))
	}
	if budget.Spent() {
		return nil, refusal.Refuse(http.StatusTooManyRequests, domain.ReasonModelBudgetSpent,
			fmt.Sprintf("platform experiment %s spent its model budget: its model calls cost $%.2f of $%.2f (model_budget_usd). Its owner raises model_budget_usd to go on", who.platformExperiment, budget.UsedUSD, budget.BudgetUSD))
	}
	return p.hold(who.org)
}

// requestedModel is the model the call names, its body left for the provider to read.
func requestedModel(r *http.Request) (string, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return "", refusal.Refuse(http.StatusRequestEntityTooLarge, refusal.ReasonTooLarge, fmt.Sprintf("a model call is at most %d bytes", maxRequestBytes))
	}
	if err != nil {
		return "", refusal.Refuse(http.StatusBadRequest, refusal.ReasonInvalidRequest, "the call's body could not be read")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var call struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &call); err != nil {
		return "", refusal.Refuse(http.StatusBadRequest, refusal.ReasonInvalidRequest, "a model call is a JSON object that names its model")
	}
	return call.Model, nil
}

// hold counts one more call of org in flight, refused once it has policy.MaxInFlightPerOrg.
func (p *Proxy) hold(org string) (release func(), err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight[org] >= p.policy.MaxInFlightPerOrg {
		return nil, refusal.Refuse(http.StatusTooManyRequests, domain.ReasonModelCallsInFlight,
			fmt.Sprintf("org %s has %d model calls in flight, as many as the platform makes for one org at once (models.max_calls_in_flight_per_org): retry once one ends", org, p.policy.MaxInFlightPerOrg))
	}
	p.inFlight[org]++
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.inFlight[org]--; p.inFlight[org] == 0 {
			delete(p.inFlight, org)
		}
	}, nil
}

// meter reads the usage of a call that spent tokens from its answer as the answer passes, and
// records it once the answer has been sent or the call has ended.
func (p *Proxy) meter(resp *http.Response) error {
	who, _ := resp.Request.Context().Value(callerKey{}).(caller)
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != messagesPath || who.agent == "" {
		return nil
	}
	resp.Body = &metered{
		ReadCloser: resp.Body,
		reading:    newUsageReader(resp.Header.Get("Content-Type")),
		record: func(u usage) {
			p.record(context.WithoutCancel(resp.Request.Context()), who, u)
		},
	}
	return nil
}

func (p *Proxy) record(ctx context.Context, who caller, u usage) {
	ctx, cancel := context.WithTimeout(ctx, recordLimit)
	defer cancel()
	err := p.usage.RecordModelCall(ctx, metricsdb.ModelCall{
		OrgID: who.org, PlatformExperimentID: who.platformExperiment, AgentID: who.agent, Provider: AnthropicProvider,
		Model: u.Model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens, CacheReadInputTokens: u.CacheReadInputTokens, At: p.now(),
	})
	if err != nil {
		p.logger.Error("model proxy: record the usage of a call", zap.Error(err),
			zap.String("platform_experiment_id", who.platformExperiment), zap.String("agent_id", who.agent))
	}
}

// failed answers a call the provider could not be reached for in the provider's own error shape,
// which the agent's harness reads. A call the agent gave up on is answered to no one.
func (p *Proxy) failed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(r.Context().Err(), context.Canceled) {
		return
	}
	p.logger.Warn("model proxy: the provider could not be reached", zap.Error(err), zap.String("path", r.URL.Path))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"the platform could not reach the model provider"}}`)
}

// metered hands each read of an answer to its usage reader and records the usage once, when the
// answer is closed: after its last byte, or when the call ended before it.
type metered struct {
	io.ReadCloser
	reading  *usageReader
	record   func(usage)
	recorded bool
}

func (m *metered) Read(b []byte) (int, error) {
	n, err := m.ReadCloser.Read(b)
	m.reading.Write(b[:n])
	return n, err
}

func (m *metered) Close() error {
	err := m.ReadCloser.Close()
	if !m.recorded {
		m.recorded = true
		if u, ok := m.reading.Usage(); ok {
			m.record(u)
		}
	}
	return err
}
