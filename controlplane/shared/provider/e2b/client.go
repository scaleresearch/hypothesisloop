// Package e2b rents E2B sandboxes. What real sandboxes taught, and the code leans on:
//
//   - A sandbox boots from a template and runs no command of its own. The adapter starts the
//     command through envd, the vendor's process inside every sandbox, under the supervisor in
//     supervise.go, which keeps the output and the state in files that envd serves.
//   - A template fixes the cores and the memory of its sandboxes, so there is one per image and
//     size, under a name made from both (template.go).
//   - envd logs every process start with its arguments and environment, and the vendor keeps
//     that log. The command and its environment therefore travel as an uploaded file, whose
//     content is not logged.
//   - The vendor's list shows a sandbox as running and nothing of what runs in it, and envd
//     answers only to the sandbox's own token, so List makes two calls for each sandbox of the
//     prefix: one for the token and one for the state file. They are made side by side, and one
//     sandbox that cannot be read does not fail the list.
//   - Whatever a sandbox listens on answers at a public address unless public traffic is turned
//     off when the sandbox is made, which Create does. envd is not behind that switch.
//   - Everything read from inside a sandbox is written by what runs there, so every such read
//     has a bound.
//   - A sandbox is killed when its lifetime ends, with everything in it. The plan caps the
//     lifetime, at one hour on the free plan.
//   - Nothing of the account is inside a sandbox.
package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/provider/vendorapi"
)

var _ provider.Provider = (*Provider)(nil)

const (
	defaultBaseURL = "https://api.e2b.app"
	requestTimeout = 30 * time.Second
	pollInterval   = time.Second

	// launchWindow bounds what Create does with a sandbox it has made: the wait to see it in
	// the list, and then the start of the command inside it.
	launchWindow = 30 * time.Second

	// maxAnswer bounds one answer of the API. The largest is a page of the list, a hundred
	// sandboxes of a few hundred bytes each.
	maxAnswer = 4 << 20
)

type Provider struct {
	cfg     Settings
	apiKey  string
	baseURL string
	// envdURL is the address of the envd of one sandbox.
	envdURL func(sandboxID string) string
	http    *http.Client
	poll    time.Duration
	window  time.Duration
	// stateRead bounds the reading of one sandbox's state in List.
	stateRead time.Duration
}

// New refuses any accelerator type: the vendor rents CPU-only sandboxes, so a shape that asks
// for an accelerator stops the process at startup instead of a job at Create.
func New(cfg Settings, acceleratorTypes []string) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(acceleratorTypes) > 0 {
		return nil, fmt.Errorf("e2b: the vendor rents no accelerators, and a shape asks for %q", acceleratorTypes[0])
	}
	key, err := cfg.apiKey()
	if err != nil {
		return nil, err
	}
	return &Provider{
		cfg:       cfg,
		apiKey:    key,
		baseURL:   defaultBaseURL,
		envdURL:   func(sandboxID string) string { return "https://49983-" + sandboxID + ".e2b.app" },
		http:      &http.Client{Timeout: requestTimeout},
		poll:      pollInterval,
		window:    launchWindow,
		stateRead: stateReadWindow,
	}, nil
}

const vendor = "e2b"

// hasStatus tells whether err is an answer with the given HTTP status, from the API or from a
// sandbox's envd.
func hasStatus(err error, status int) bool {
	var failure *vendorapi.StatusError
	return errors.As(err, &failure) && failure.Status == status
}

// call sends one JSON request to the API and decodes a 2xx answer into out. A nil out is for an
// answer with nothing to read.
func (p *Provider) call(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("e2b: encode %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, payload)
	if err != nil {
		return nil, fmt.Errorf("e2b: build %s %s: %w", method, path, err)
	}
	req.Header.Set("X-API-Key", p.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	raw, header, err := vendorapi.Do(p.http, req, vendor, maxAnswer)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return header, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("e2b: decode %s %s: %w", method, path, err)
	}
	return header, nil
}
