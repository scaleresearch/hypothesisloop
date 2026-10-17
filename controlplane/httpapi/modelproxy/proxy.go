package modelproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// Prefix is where the proxy is served, the provider's name after it.
const Prefix = "/model/"

// Store says whom the platform runs. *db.Store satisfies it.
type Store interface {
	Hosted(ctx context.Context, platformExpID, agentID string) (bool, error)
}

// Usage stores what the provider counted for a call.
type Usage interface {
	RecordModelCall(ctx context.Context, call metricsdb.ModelCall) error
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
	upstream *url.URL
	reverse  *httputil.ReverseProxy
	logger   *zap.Logger
	now      func() time.Time
}

// New is the proxy to the Anthropic API at upstream, which makes every call with oauthToken, a
// Claude subscription token.
func New(store Store, usage Usage, upstream, oauthToken string, logger *zap.Logger) (*Proxy, error) {
	if store == nil || usage == nil || logger == nil {
		return nil, errors.New("modelproxy.New: a store, a usage store and a logger are required")
	}
	if oauthToken == "" {
		return nil, errors.New("modelproxy.New: the provider's credential is required")
	}
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" || (target.Scheme != "https" && target.Scheme != "http") {
		return nil, errors.New("modelproxy.New: the upstream is an http(s) address")
	}
	p := &Proxy{store: store, usage: usage, upstream: target, logger: logger, now: time.Now}
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
	hosted, err := p.store.Hosted(ctx, principal.PlatformExperimentID, principal.AgentID)
	if err != nil {
		return caller{}, auth.Internal(p.logger, "model proxy: read whether the platform runs a caller", err, zap.String("platform_experiment_id", principal.PlatformExperimentID))
	}
	if !hosted {
		return caller{}, auth.Forbidden("the platform does not run " + who.agent + " of platform experiment " + who.platformExperiment + ", so its model calls are not made through the platform")
	}
	return who, nil
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
