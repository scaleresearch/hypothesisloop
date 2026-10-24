package modelproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// OpenAI is OpenAI's models on a ChatGPT subscription, called at URL as the Codex CLI calls them,
// each call made with the credential Credentials holds at the time.
type OpenAI struct {
	URL         string
	Credentials ChatGPTCredentials
}

// ChatGPTCredentials is the ChatGPT credential, fresh, and made due once the ChatGPT backend
// refused its access token. *chatgpt.Credentials satisfies it.
type ChatGPTCredentials interface {
	Fresh(ctx context.Context) (domain.ChatGPTCredential, error)
	Expire(ctx context.Context, accessToken string) error
}

// maxRefusalBytes bounds the body of a refusal read to translate it.
const maxRefusalBytes = 1 << 20

type shapeKey struct{}

// anthropicError is a call the proxy answers itself in Anthropic's error shape, as the provider
// would have.
type anthropicError struct {
	status        int
	kind, message string
}

func (e *anthropicError) Error() string { return e.message }

// openAIProvider is o as the proxy calls it: each call translated to a Responses call and its
// answer back.
func (p *Proxy) openAIProvider(o *OpenAI) (provider, error) {
	if o.Credentials == nil {
		return provider{}, errors.New("the provider's credential is required")
	}
	target, err := upstreamURL(o.URL)
	if err != nil {
		return provider{}, err
	}
	reverse := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			to := *target
			r.Out.URL, r.Out.Host = &to, ""
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode == http.StatusUnauthorized {
				p.expire(o, resp.Request)
			}
			translateAnswer(resp, p.logger)
			return p.meter(resp)
		},
		ErrorHandler: p.failed,
		ErrorLog:     zap.NewStdLog(p.logger),
	}
	return provider{reverse: reverse, onlyMessages: true, ownModelOnly: true, usageAtEnd: true,
		prepare: func(r *http.Request, model string) (*http.Request, error) {
			return o.prepare(r, model, p.logger)
		}}, nil
}

// expire makes the credential the ChatGPT backend refused the call out due, so the next call
// renews it.
func (p *Proxy) expire(o *OpenAI, out *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(out.Context()), recordLimit)
	defer cancel()
	if err := o.Credentials.Expire(ctx, strings.TrimPrefix(out.Header.Get("Authorization"), "Bearer ")); err != nil {
		p.logger.Error("model proxy: make the refused ChatGPT credential due", zap.Error(err))
	}
}

// prepare makes r, a Messages call on model, the Responses call of it with the platform's
// credential and nothing of the caller's.
func (o *OpenAI) prepare(r *http.Request, model string, logger *zap.Logger) (*http.Request, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, &anthropicError{http.StatusBadRequest, "invalid_request_error", "the call's body could not be read"}
	}
	translated, how, err := toResponses(body, model, r.Header.Get("X-Claude-Code-Session-Id"))
	if err != nil {
		return nil, &anthropicError{http.StatusBadRequest, "invalid_request_error", "the call has no counterpart for an openai model: " + err.Error()}
	}
	cred, err := o.Credentials.Fresh(r.Context())
	if err != nil {
		logger.Error("model proxy: the ChatGPT credential", zap.Error(err))
		return nil, &anthropicError{http.StatusBadGateway, "api_error", "the platform could not renew its ChatGPT credential"}
	}
	out := r.WithContext(context.WithValue(r.Context(), shapeKey{}, how))
	out.Header = http.Header{
		"Authorization":      {"Bearer " + cred.AccessToken},
		"Chatgpt-Account-Id": {cred.AccountID},
		"Openai-Beta":        {"responses=experimental"},
		"Originator":         {"codex_cli_rs"},
		"Accept":             {"text/event-stream"},
		"Content-Type":       {"application/json"},
	}
	out.Body = io.NopCloser(bytes.NewReader(translated))
	out.ContentLength = int64(len(translated))
	out.GetBody = nil
	return out, nil
}

// translateAnswer makes the ChatGPT backend's answer to a Responses call the Anthropic answer of
// the Messages call it was made for: a stream translated as it passes, a whole message read to
// its end, or an error.
func translateAnswer(resp *http.Response, logger *zap.Logger) {
	how, _ := resp.Request.Context().Value(shapeKey{}).(shape)
	// The ChatGPT backend's cookies are the platform's session, none of the caller's.
	resp.Header.Del("Set-Cookie")
	resp.Header.Del("Content-Length")
	resp.Header.Del("Content-Encoding")
	resp.ContentLength = -1
	upstream := resp.Body
	switch {
	case resp.StatusCode != http.StatusOK:
		refused, _ := io.ReadAll(io.LimitReader(upstream, maxRefusalBytes))
		_ = upstream.Close()
		now := time.Now()
		status, body, resetsAt := refusedAnswer(resp.StatusCode, refused, now)
		resp.StatusCode, resp.Status = status, fmt.Sprintf("%d %s", status, http.StatusText(status))
		setBody(resp, "application/json", body)
		if !resetsAt.IsZero() {
			refusedUntil(resp.Header, resetsAt, now)
		}
	case how.stream:
		resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
		resp.Body = newTranslatedStream(upstream, how, logger)
	default:
		status, message := wholeMessage(upstream, how, logger)
		_ = upstream.Close()
		resp.StatusCode, resp.Status = status, fmt.Sprintf("%d %s", status, http.StatusText(status))
		setBody(resp, "application/json", message)
	}
}

// refusedUntil says on h, as Anthropic's API says it of a Claude subscription, that the model's
// account refuses calls until resetsAt: Claude Code reports the limit, and the harness waits it out.
func refusedUntil(h http.Header, resetsAt, now time.Time) {
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(resetsAt.Unix(), 10))
	h.Set("Retry-After", strconv.Itoa(max(0, int(resetsAt.Sub(now).Seconds()))))
}

func setBody(resp *http.Response, contentType string, body []byte) {
	resp.Header.Set("Content-Type", contentType)
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.ContentLength = int64(len(body))
	resp.Body = io.NopCloser(bytes.NewReader(body))
}
