// Package runpod rents pods on Runpod through its REST API v2. What real pods taught, and the
// code leans on:
//
//   - A command that ends is started again in the same container about two seconds later, with
//     any exit code, for as long as the pod is meant to run, and the pod stays RUNNING. Every
//     command runs under the supervisor in supervise.go, which keeps the container alive after
//     the command and says so in the pod's name.
//   - The vendor's stop removes the container, and its output with it, so Stop does not use it:
//     it marks the name, which the supervisor reads (stop.go).
//   - A pod is RUNNING from the moment it is created, image pull included, so the supervisor
//     marks the name once the command has started.
//   - The key the vendor puts in every container reads and renames that pod alone.
//   - Only GPU pods join the global network, and they reach each other at 100 Mbps.
//   - A pod on the global network gets its private address at create, and the interface it is on
//     comes up inside the container some seconds after the container starts.
package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider"
	"github.com/scaleresearch/hypothesisloop/controlplane/clients/provider/vendorapi"
)

var _ provider.Provider = (*Provider)(nil)

const (
	defaultBaseURL = "https://api.runpod.io"
	requestTimeout = 30 * time.Second
	vendor         = "runpod"
	// maxAnswer bounds an answer: the largest is the whole list of the account's pods, each with
	// its environment and command.
	maxAnswer = 16 << 20
	// architecture is the only one the vendor rents machines of.
	architecture = "amd64"
)

type Provider struct {
	log     func(format string, args ...any)
	cfg     Settings
	apiKey  string
	baseURL string
	http    *http.Client
	// logs reads log streams, which stay open: it has no timeout of its own.
	logs *http.Client
	// logQuiet is logQuietFor, shorter in tests.
	logQuiet time.Duration
	poll     time.Duration
}

// New refuses settings that cannot rent one of acceleratorTypes, the catalog strings the caller
// will ask for, so that a missing entry stops the process at startup instead of a job at Create.
func New(cfg Settings, key string, acceleratorTypes []string, log func(format string, args ...any)) (*Provider, error) {
	if log == nil {
		return nil, errors.New("runpod: a log is required")
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
		return nil, errors.New("runpod: an API key is required")
	}
	return &Provider{
		log:      log,
		cfg:      cfg,
		apiKey:   key,
		baseURL:  defaultBaseURL,
		http:     &http.Client{Timeout: requestTimeout},
		logs:     &http.Client{},
		logQuiet: logQuietFor,
		poll:     time.Second,
	}, nil
}

// Account is the account the settings name (provider.Accounted).
func (p *Provider) Account() string { return p.cfg.AccountID }

func (p *Provider) Architectures() []string { return []string{architecture} }

// statusError is a non-2xx answer from the API, with the explanation its problem body carries,
// empty when it carried none.
type statusError struct {
	*vendorapi.StatusError
	Detail string
}

// call sends one authenticated JSON request and decodes a 2xx answer into out, which may be nil
// for an answer with no body.
func (p *Provider) call(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("runpod: encode %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, payload)
	if err != nil {
		return fmt.Errorf("runpod: build %s %s: %w", method, path, err)
	}
	p.authorize(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	raw, _, err := vendorapi.Do(p.http, req, vendor, maxAnswer)
	if err != nil {
		return refusal(err)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("runpod: decode %s %s: %w", method, path, err)
	}
	return nil
}

func (p *Provider) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Accept", "application/json")
}

// refusal is err with the problem body of a non-2xx answer read.
func refusal(err error) error {
	var failed *vendorapi.StatusError
	if !errors.As(err, &failed) {
		return err
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal([]byte(failed.Body), &problem)
	return &statusError{StatusError: failed, Detail: problem.Detail}
}

func status(err error) int {
	var failure *statusError
	if errors.As(err, &failure) {
		return failure.Status
	}
	return 0
}
