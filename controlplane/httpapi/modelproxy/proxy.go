package modelproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
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

// Store says whom the platform runs, and what their model resolves from. *db.Store satisfies it.
type Store interface {
	ProxiedModel(ctx context.Context, platformExpID, agentID string) (*domain.ModelChoice, error)
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

// Providers are the providers whose models the proxy calls, each nil when the platform does not
// serve it.
type Providers struct {
	Anthropic  *Anthropic
	OpenRouter *OpenRouter
	OpenAI     *OpenAI
}

// Anthropic is Claude's API at URL, every call made with OAuthToken, a Claude subscription token.
type Anthropic struct {
	URL, OAuthToken string
	// Models are the models a call may name, each call priced at its model's price.
	Models domain.ModelCatalog
}

// OpenRouter is OpenRouter's Anthropic-compatible API at URL, every call made with APIKey. A call
// may name any model OpenRouter serves, and costs what OpenRouter's answer says it did.
type OpenRouter struct {
	URL, APIKey string
}

const (
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
	// finishLimit bounds how long the answer of a provider whose usage comes at its end is read on
	// after its caller hung up, for that usage.
	finishLimit = 5 * time.Minute
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

// provider is one provider as the proxy calls it.
type provider struct {
	reverse *httputil.ReverseProxy
	// admit, when set, refuses a model a call to the provider may not name.
	admit func(model string) error
	// ownModelOnly is true of a provider a call to which names the caller's model itself: the
	// harness sets every model Claude Code calls to it.
	ownModelOnly bool
	// usageAtEnd is true of a provider whose answer states its usage only at its end: one the
	// caller hung up on is read on to its end, for at most finishLimit, and the provider's
	// generation goes on meanwhile.
	usageAtEnd bool
	// cost is what a call on model cost, as its answer counted it.
	cost func(model string, u usage) (float64, error)
	// onlyMessages is true of a provider that serves messagesPath alone.
	onlyMessages bool
	// prepare, when set, makes an admitted call on model the provider's own call.
	prepare func(r *http.Request, model string) (*http.Request, error)
}

// Proxy serves Prefix + the name of each provider it calls, behind auth.Gate.
type Proxy struct {
	store             Store
	usage             Usage
	budgets           Budgets
	providers         map[string]provider
	defaults          domain.ModelDefaults
	maxInFlightPerOrg int
	logger            *zap.Logger
	now               func() time.Time

	// inFlight counts each org's calls this process is making: live connections, which no store
	// holds.
	mu       sync.Mutex
	inFlight map[string]int
}

// New is the proxy to providers, which makes at most maxInFlightPerOrg calls of one org at once,
// each on the model of its caller, resolved with defaults.
func New(store Store, usage Usage, budgets Budgets, providers Providers, defaults domain.ModelDefaults, maxInFlightPerOrg int, logger *zap.Logger) (*Proxy, error) {
	if store == nil || usage == nil || budgets == nil || logger == nil {
		return nil, errors.New("modelproxy.New: a store, a usage store, the budgets and a logger are required")
	}
	if maxInFlightPerOrg <= 0 {
		return nil, errors.New("modelproxy.New: the calls an org may have in flight are positive")
	}
	p := &Proxy{store: store, usage: usage, budgets: budgets, providers: map[string]provider{}, defaults: defaults, maxInFlightPerOrg: maxInFlightPerOrg,
		logger: logger, now: time.Now, inFlight: map[string]int{}}
	if a := providers.Anthropic; a != nil {
		if err := a.Models.Validate(domain.ModelProviderAnthropic); err != nil {
			return nil, fmt.Errorf("modelproxy.New: anthropic: %w", err)
		}
		reverse, err := p.reverseProxy(a.URL, a.OAuthToken, true)
		if err != nil {
			return nil, fmt.Errorf("modelproxy.New: anthropic: %w", err)
		}
		p.providers[domain.ModelProviderAnthropic] = provider{reverse: reverse, admit: a.admit, cost: a.cost}
	}
	if o := providers.OpenRouter; o != nil {
		reverse, err := p.reverseProxy(o.URL, o.APIKey, false)
		if err != nil {
			return nil, fmt.Errorf("modelproxy.New: openrouter: %w", err)
		}
		p.providers[domain.ModelProviderOpenRouter] = provider{reverse: reverse, cost: o.cost, ownModelOnly: true, usageAtEnd: true}
	}
	if o := providers.OpenAI; o != nil {
		openAI, err := p.openAIProvider(o)
		if err != nil {
			return nil, fmt.Errorf("modelproxy.New: openai: %w", err)
		}
		p.providers[domain.ModelProviderOpenAI] = openAI
	}
	return p, nil
}

func (a *Anthropic) admit(model string) error {
	if _, ok := a.Models[model]; !ok {
		return refusal.Refuse(http.StatusForbidden, domain.ReasonModelNotServed,
			fmt.Sprintf("model %q is not one the platform serves: name one of %s", model, strings.Join(a.Models.Names(), ", ")))
	}
	return nil
}

func (a *Anthropic) cost(model string, u usage) (float64, error) {
	return a.Models[model].CostUSD(domain.ModelTokens{Input: u.InputTokens, Output: u.OutputTokens, CacheWrite: u.CacheCreationInputTokens, CacheRead: u.CacheReadInputTokens}), nil
}

func (*OpenRouter) cost(_ string, u usage) (float64, error) {
	if u.CostUSD == nil {
		return 0, errors.New("the answer states no cost")
	}
	return *u.CostUSD, nil
}

// reverseProxy passes a call on to the provider at upstream, made with credential, under the
// Anthropic OAuth beta when oauth.
func (p *Proxy) reverseProxy(upstream, credential string, oauth bool) (*httputil.ReverseProxy, error) {
	if credential == "" {
		return nil, errors.New("the provider's credential is required")
	}
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return nil, errors.New("the upstream is an http(s) address")
	}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			withCredential(r.Out.Header, credential, oauth)
		},
		// Each event of a streamed answer reaches the agent as it arrives.
		FlushInterval:  -1,
		ModifyResponse: p.meter,
		ErrorHandler:   p.failed,
		ErrorLog:       zap.NewStdLog(p.logger),
	}, nil
}

// withCredential makes the outgoing headers carry the platform's credential and nothing of the
// caller's: its platform token is the caller's alone and never leaves the platform. The OAuth beta
// goes first with oauth and to no other provider. The transport asks for and undoes the
// compression itself, so the proxy reads the usage of every answer.
func withCredential(h http.Header, credential string, oauth bool) {
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Accept-Encoding"} {
		h.Del(name)
	}
	h.Set("Authorization", "Bearer "+credential)
	var betas []string
	if oauth {
		betas = append(betas, oauthBeta)
	}
	for _, value := range h.Values("Anthropic-Beta") {
		for _, beta := range strings.Split(value, ",") {
			if beta = strings.TrimSpace(beta); beta != "" && beta != oauthBeta {
				betas = append(betas, beta)
			}
		}
	}
	h.Del("Anthropic-Beta")
	if len(betas) > 0 {
		h.Set("Anthropic-Beta", strings.Join(betas, ","))
	}
}

// caller is whom a call is made and metered for, and the model it runs on.
type caller struct {
	org, platformExperiment, agent string
	model                          domain.ModelRef
}

// call is one call the proxy makes: for whom, to which provider's endpoint and on which model.
type call struct {
	caller
	provider, endpoint, model string
}

type callKey struct{}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, _ := strings.CutPrefix(r.URL.Path, Prefix)
	name, endpoint, _ := strings.Cut(rest, "/")
	endpoint = "/" + endpoint
	to, ok := p.providers[name]
	if !ok {
		auth.WriteError(w, p.logger, auth.NotFound("model provider "+name+": the platform serves "+strings.Join(slices.Sorted(maps.Keys(p.providers)), ", ")+", each at "+Prefix+"<provider>"))
		return
	}
	if method, ok := served[endpoint]; !ok || r.Method != method || (to.onlyMessages && endpoint != messagesPath) {
		auth.WriteError(w, p.logger, auth.NotFound(r.Method+" "+endpoint+": the model proxy serves POST "+messagesPath+" and POST /v1/messages/count_tokens"))
		return
	}
	who, err := p.authorize(r.Context())
	if err != nil {
		auth.WriteError(w, p.logger, err)
		return
	}
	model, release, err := p.admit(r, name, to, who)
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
	upstream := r.Context()
	if to.usageAtEnd {
		var finish context.CancelFunc
		upstream, finish = context.WithCancel(context.WithoutCancel(upstream))
		defer finish()
		defer context.AfterFunc(r.Context(), func() { time.AfterFunc(finishLimit, finish) })()
	}
	out := r.Clone(context.WithValue(upstream, callKey{}, call{caller: who, provider: name, endpoint: endpoint, model: model}))
	out.URL.Path = endpoint
	out.URL.RawPath = ""
	if to.prepare != nil {
		if out, err = to.prepare(out, model); err != nil {
			var failed *anthropicError
			if errors.As(err, &failed) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(failed.status)
				_, _ = w.Write(errorBody(failed.kind, failed.message))
				return
			}
			auth.WriteError(w, p.logger, err)
			return
		}
	}
	to.reverse.ServeHTTP(w, out)
}

// authorize says whom the caller's call is made for and on which model: an agent or the
// coordinator of a platform experiment, while auth.Gate takes its token, and only one the platform
// runs, on its model as its instructions resolve it. An agent run elsewhere brings its own model.
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
	choice, err := p.store.ProxiedModel(ctx, principal.PlatformExperimentID, principal.AgentID)
	if err != nil {
		return caller{}, auth.Internal(p.logger, "model proxy: read whether the platform runs a caller", err, zap.String("platform_experiment_id", principal.PlatformExperimentID))
	}
	if choice == nil {
		return caller{}, auth.Forbidden("the platform does not run " + who.agent + " of platform experiment " + who.platformExperiment + ", so its model calls are not made through the platform")
	}
	if who.model, err = domain.ParseModelRef(choice.Model(p.defaults)); err != nil {
		return caller{}, auth.Internal(p.logger, "model proxy: resolve a caller's model", err, zap.String("platform_experiment_id", who.platformExperiment), zap.String("agent_id", who.agent))
	}
	return who, nil
}

// admit is the platform's policy on a call it pays for, applied here and nowhere else: the call
// goes to the provider of the caller's model, names that model where the provider serves the
// caller's own alone and otherwise one the provider may be called on, the platform experiment's model calls have not
// cost its model budget, and its org has a call in flight to spare. It answers the model, and
// release ends the call's hold on its org's. The caller's token, which outlives its run by
// domain.AgentWindDown or domain.CoordinatorWindDown for its last turn, is auth.Gate's to refuse
// after.
func (p *Proxy) admit(r *http.Request, name string, to provider, who caller) (model string, release func(), err error) {
	if model, err = requestedModel(r); err != nil {
		return "", nil, err
	}
	if who.model.Provider() != name {
		return "", nil, refusal.Refuse(http.StatusForbidden, domain.ReasonModelNotTheCallers,
			fmt.Sprintf("%s runs on %s, so its model calls go to %s%s and none to %s%s", who.agent, who.model, Prefix, who.model.Provider(), Prefix, name))
	}
	if to.ownModelOnly && model != who.model.ID() {
		return "", nil, refusal.Refuse(http.StatusForbidden, domain.ReasonModelNotTheCallers,
			fmt.Sprintf("%s runs on %s, so its calls to %s%s name model %q and no other, not %q", who.agent, who.model, Prefix, name, who.model.ID(), model))
	}
	if to.admit != nil {
		if err := to.admit(model); err != nil {
			return "", nil, err
		}
	}
	budget, err := p.budgets.ModelBudget(r.Context(), who.platformExperiment)
	if err != nil {
		return "", nil, auth.Internal(p.logger, "model proxy: read a platform experiment's model budget", err, zap.String("platform_experiment_id", who.platformExperiment))
	}
	if budget.Spent() {
		return "", nil, refusal.Refuse(http.StatusTooManyRequests, domain.ReasonModelBudgetSpent,
			fmt.Sprintf("platform experiment %s spent its model budget: its model calls cost $%.2f of $%.2f (model_budget_usd). Its owner raises model_budget_usd to go on", who.platformExperiment, budget.UsedUSD, budget.BudgetUSD))
	}
	release, err = p.hold(who.org)
	return model, release, err
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

// hold counts one more call of org in flight, refused once it has maxInFlightPerOrg.
func (p *Proxy) hold(org string) (release func(), err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight[org] >= p.maxInFlightPerOrg {
		return nil, refusal.Refuse(http.StatusTooManyRequests, domain.ReasonModelCallsInFlight,
			fmt.Sprintf("org %s has %d model calls in flight, as many as the platform makes for one org at once (models.max_calls_in_flight_per_org): retry once one ends", org, p.maxInFlightPerOrg))
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
	made, _ := resp.Request.Context().Value(callKey{}).(call)
	if resp.StatusCode != http.StatusOK || made.endpoint != messagesPath {
		return nil
	}
	resp.Body = &metered{
		ReadCloser: resp.Body,
		toEnd:      p.providers[made.provider].usageAtEnd,
		reading:    newUsageReader(resp.Header.Get("Content-Type")),
		record: func(u usage) {
			p.record(context.WithoutCancel(resp.Request.Context()), made, u)
		},
	}
	return nil
}

// record stores the usage of made with its cost. A call whose cost cannot be told was made all
// the same: its usage is recorded at no cost, and the failure logged.
func (p *Proxy) record(ctx context.Context, made call, u usage) {
	fields := []zap.Field{zap.String("platform_experiment_id", made.platformExperiment), zap.String("agent_id", made.agent),
		zap.String("provider", made.provider), zap.String("model", made.model)}
	cost, err := p.providers[made.provider].cost(made.model, u)
	if err != nil {
		p.logger.Error("model proxy: price a call", append(fields, zap.Error(err))...)
	}
	ctx, cancel := context.WithTimeout(ctx, recordLimit)
	defer cancel()
	err = p.usage.RecordModelCall(ctx, metricsdb.ModelCall{
		OrgID: made.org, PlatformExperimentID: made.platformExperiment, AgentID: made.agent, Provider: made.provider,
		Model: made.model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens, CacheReadInputTokens: u.CacheReadInputTokens, CostUSD: cost, At: p.now(),
	})
	if err != nil {
		p.logger.Error("model proxy: record the usage of a call", append(fields, zap.Error(err))...)
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
// answer is closed: after its last byte, or when the call ended before it. An answer whose usage
// comes at its end is first read to its end, which finishLimit bounds once its caller hung up.
type metered struct {
	io.ReadCloser
	toEnd    bool
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
	if m.toEnd && !m.recorded {
		_, _ = io.Copy(io.Discard, m)
	}
	err := m.ReadCloser.Close()
	if !m.recorded {
		m.recorded = true
		if u, ok := m.reading.Usage(); ok {
			m.record(u)
		}
	}
	return err
}
