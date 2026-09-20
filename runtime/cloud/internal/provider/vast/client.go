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
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

var _ provider.Provider = (*Provider)(nil)

const (
	defaultBaseURL = "https://console.vast.ai"
	requestTimeout = 30 * time.Second
	maxErrorBody   = 512
	pollInterval   = time.Second
)

type Provider struct {
	cfg     Settings
	apiKey  string
	baseURL string
	http    *http.Client
	poll    time.Duration
	logWait time.Duration
	shuffle func([]offer)
}

// New refuses settings that cannot rent one of acceleratorTypes, the catalog strings the caller
// will ask for, so that a missing entry stops the process at startup instead of a job at Create.
func New(cfg Settings, acceleratorTypes []string) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	for _, catalogType := range acceleratorTypes {
		if _, err := cfg.accelerator(catalogType); err != nil {
			return nil, err
		}
	}
	key, err := cfg.apiKey()
	if err != nil {
		return nil, err
	}
	return &Provider{
		cfg:     cfg,
		apiKey:  key,
		baseURL: defaultBaseURL,
		http:    &http.Client{Timeout: requestTimeout},
		poll:    pollInterval,
		logWait: logDeliveryTimeout,
		shuffle: shuffleOffers,
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
			Error   string `json:"error"`
			Message string `json:"msg"`
		}
		_ = json.Unmarshal(raw, &failure)
		return nil, &statusError{Method: method, Path: path, Status: resp.StatusCode, Code: failure.Error, Message: failure.Message, Body: truncate(raw)}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("vast: decode %s %s: %w", method, path, err)
	}
	return resp.Header, nil
}

func truncate(raw []byte) string {
	if len(raw) > maxErrorBody {
		return string(raw[:maxErrorBody])
	}
	return string(raw)
}
