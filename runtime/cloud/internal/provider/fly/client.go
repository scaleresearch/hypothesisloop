// Package fly rents Fly.io Machines. What real Machines taught, and the code leans on:
//
//   - A Machine's output is read from the log search the vendor's own CLI uses. It holds the
//     command's lines mixed with those of the vendor's init, its ssh server and the kernel, all
//     under one source, which is why every command runs under the supervisor in supervise.go:
//     only lines carrying its prefix are the command's.
//   - That log arrives a moment after it was written, so a stopped Machine is exited only once the
//     supervisor's end mark is in it (instances.go).
//   - The log keeps a few hundred lines of a burst and drops the rest: 3000 lines written at
//     once left 348.
//   - A Machine with restart policy "no" stays stopped after its command ends, and a stopped
//     Machine is billed for its root filesystem only.
//   - The signal call reaches the Machine's main process and nothing follows it. The vendor's
//     stop kills after a timeout, so Stop does not use it.
//   - Nothing of the account is inside a Machine: its API socket answers unauthorized. Machines
//     of one organization do share a private network and its DNS.
package fly

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
	defaultMachinesURL = "https://api.machines.dev"
	// defaultLogsURL is the API the vendor's CLI reads logs from. The Machines API has no logs.
	defaultLogsURL = "https://api.fly.io"
	requestTimeout = 30 * time.Second
	maxErrorBody   = 512
	pollInterval   = time.Second
)

type Provider struct {
	cfg         Settings
	token       string
	machinesURL string
	logsURL     string
	http        *http.Client
	poll        time.Duration
}

// New refuses any accelerator type: the adapter rents CPU-only Machines, so a shape that asks
// for an accelerator stops the process at startup instead of a job at Create.
func New(cfg Settings, acceleratorTypes []string) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(acceleratorTypes) > 0 {
		return nil, fmt.Errorf("fly: this adapter rents no accelerators, and a shape asks for %q", acceleratorTypes[0])
	}
	token, err := cfg.apiToken()
	if err != nil {
		return nil, err
	}
	return &Provider{
		cfg:         cfg,
		token:       token,
		machinesURL: defaultMachinesURL,
		logsURL:     defaultLogsURL,
		http:        &http.Client{Timeout: requestTimeout},
		poll:        pollInterval,
	}, nil
}

// statusError is a non-2xx answer from the API.
type statusError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("fly: %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func (p *Provider) machinesPath(suffix string) string {
	return p.machinesURL + "/v1/apps/" + p.cfg.App + "/machines" + suffix
}

// call sends one authenticated JSON request and decodes a 2xx answer into out. A nil out is for
// an answer with nothing to read.
func (p *Provider) call(ctx context.Context, method, url string, body, out any) (http.Header, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("fly: encode %s %s: %w", method, url, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return nil, fmt.Errorf("fly: build %s %s: %w", method, url, err)
	}
	req.Header.Set("Authorization", p.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fly: %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fly: read %s %s: %w", method, url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &statusError{Method: method, Path: req.URL.Path, Status: resp.StatusCode, Body: truncate(raw)}
	}
	if out == nil {
		return resp.Header, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("fly: decode %s %s: %w", method, url, err)
	}
	return resp.Header, nil
}

func truncate(raw []byte) string {
	if len(raw) > maxErrorBody {
		return string(raw[:maxErrorBody])
	}
	return string(raw)
}
