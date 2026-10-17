// Package vast rents instances on vast.ai. What real instances taught, and the code leans on:
//
//   - Every rentable offer has a GPU, so a spec with AcceleratorCount 0 rents the cheapest machine
//     of any GPU model and leaves the GPU unused.
//   - A command that ends is started again by the host about half a minute later, with any exit
//     code, for as long as the instance is meant to run. Only a stopped instance stays exited,
//     which is why every command runs under the supervisor in supervise.go.
//   - The vendor's stop sends the command SIGTERM and kills it within about a second, so Stop
//     does not use it: it marks the label, which the supervisor reads (stop.go).
//   - The status an instance shows is what its host last reported, which can be minutes old.
//   - Container output survives restarts and stops and is read through an upload the host makes
//     on request, one to a few seconds later.
package vast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/cloud/provider"
)

var _ provider.Provider = (*Provider)(nil)

const (
	defaultBaseURL = "https://console.vast.ai"
	requestTimeout = 30 * time.Second
	maxErrorBody   = 512
	pollInterval   = time.Second
	// tooManyWait is how long a request the API answered with 429 and no time to wait is not
	// sent again.
	tooManyWait = time.Minute
)

type Provider struct {
	log     func(format string, args ...any)
	cfg     Settings
	apiKey  string
	baseURL string
	http    *http.Client
	poll    time.Duration
	logWait time.Duration
	shuffle func([]offer)
	held    *held
	limited *limited
}

// limited is what the API refused with HTTP 429, by method and path, and until when. Such a
// request is answered with that refusal until then, without being sent. The offer search is
// limited by a quota of rows a day, which a refusal names with how long until it has room again.
type limited struct {
	mu    sync.Mutex
	until map[string]refusal
}

type refusal struct {
	until time.Time
	err   *statusError
}

// refused is the refusal that still stands for a request, if one does.
func (l *limited) refused(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.until[key]
	if !ok {
		return nil
	}
	if !time.Now().Before(r.until) {
		delete(l.until, key)
		return nil
	}
	return r
}

func (r refusal) Error() string {
	return fmt.Sprintf("%v; not asked again until %s", r.err, r.until.Format(time.RFC3339))
}

func (r refusal) Unwrap() error { return r.err }

// New refuses settings that cannot rent one of acceleratorTypes, the catalog strings the caller
// will ask for, so that a missing entry stops the process at startup instead of a job at Create.
func New(cfg Settings, key string, acceleratorTypes []string, log func(format string, args ...any)) (*Provider, error) {
	if log == nil {
		return nil, errors.New("vast: a log is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	for _, catalogType := range acceleratorTypes {
		if _, err := cfg.accelerator(catalogType); err != nil {
			return nil, err
		}
	}
	if key == "" {
		return nil, errors.New("vast: an API key is required")
	}
	return &Provider{
		log:     log,
		cfg:     cfg,
		apiKey:  key,
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: requestTimeout},
		poll:    pollInterval,
		logWait: logDeliveryTimeout,
		shuffle: shuffleOffers,
		held:    &held{offers: map[int64]holder{}},
		limited: &limited{until: map[string]refusal{}},
	}, nil
}

// statusError is a non-2xx answer from the API. Code is the API's own error name
// ("no_such_instance") and Message its explanation, empty when the body carried none.
type statusError struct {
	Method  string
	Path    string
	Status  int
	Code    string
	Message string
	Body    string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("vast: %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// call sends one authenticated JSON request and decodes a 2xx answer into out.
func (p *Provider) call(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	key := method + " " + path
	if err := p.limited.refused(key); err != nil {
		return nil, err
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("vast: encode %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, payload)
	if err != nil {
		return nil, fmt.Errorf("vast: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vast: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("vast: read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var failure struct {
			Error      string  `json:"error"`
			Message    string  `json:"msg"`
			RetryAfter float64 `json:"retry_after"`
		}
		_ = json.Unmarshal(raw, &failure)
		refused := &statusError{Method: method, Path: path, Status: resp.StatusCode, Code: failure.Error, Message: failure.Message, Body: truncate(raw)}
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, p.limit(key, refused, retryAfter(resp.Header, failure.RetryAfter))
		}
		return nil, refused
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("vast: decode %s %s: %w", method, path, err)
	}
	return resp.Header, nil
}

// limit keeps the API's 429 to a request for wait, and says so once.
func (p *Provider) limit(key string, refused *statusError, wait time.Duration) error {
	r := refusal{until: time.Now().Add(wait), err: refused}
	p.limited.mu.Lock()
	p.limited.until[key] = r
	p.limited.mu.Unlock()
	p.log("the API answered %s with HTTP 429 (%s): it is not asked again until %s", key, refused.Body, r.until.Format(time.RFC3339))
	return r
}

// retryAfter is how long the API asked to wait: its Retry-After header in seconds, else the
// retry_after of its body, else tooManyWait.
func retryAfter(header http.Header, inBody float64) time.Duration {
	if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if inBody > 0 {
		return time.Duration(inBody * float64(time.Second))
	}
	return tooManyWait
}

func truncate(raw []byte) string {
	if len(raw) > maxErrorBody {
		return string(raw[:maxErrorBody])
	}
	return string(raw)
}
