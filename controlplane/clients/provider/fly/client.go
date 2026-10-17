// Package fly rents Fly.io Machines. What real Machines taught, and the code leans on:
//
//   - A Machine's output is read from the log search the vendor's own CLI uses. It holds the
//     command's lines mixed with those of the vendor's init, its ssh server and the kernel, all
//     under one source, which is why every command runs under the supervisor in supervise.go:
//     only lines carrying its prefix are the command's.
//   - That log arrives a moment after it was written, so a stopped Machine is exited only once the
//     supervisor's end mark is in it (instances.go).
//   - The log keeps a few hundred lines of a burst and drops the rest: 3000 lines written at
//     once left 348, and 2500 written at twenty a second were all kept. One read gives the last
//     500 entries at most and there is no reading further back.
//   - A Machine with restart policy "no" stays stopped after its command ends, and a stopped
//     Machine is billed for its root filesystem only.
//   - The signal call reaches the Machine's main process and nothing follows it. The vendor's
//     stop kills after a timeout, so Stop does not use it.
//   - A Machine's API socket answers unauthorized for other Machines and for the account. It
//     lists its app's secrets, writes its own metadata, suspends itself and mints an OIDC token
//     naming its app and itself. Machines of one app share a private network, so each Machine
//     has an app of its own (apps.go), which holds no secrets.
package fly

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
	defaultMachinesURL = "https://api.machines.dev"
	// defaultLogsURL is the API the vendor's CLI reads logs from. The Machines API has no logs.
	defaultLogsURL = "https://api.fly.io"
	requestTimeout = 30 * time.Second
	pollInterval   = time.Second

	// confirmWindow is how long Create waits to see the Machine it made in the list.
	confirmWindow = 30 * time.Second

	// maxAnswer bounds one answer of either API. The largest are the list, a few kilobytes a
	// Machine, and a page of the log, whose lines the command writes.
	maxAnswer = 32 << 20
)

type Provider struct {
	log         func(format string, args ...any)
	cfg         Settings
	token       string
	machinesURL string
	logsURL     string
	http        *http.Client
	poll        time.Duration
	confirm     time.Duration
	// suffix makes the random part of a new app's name.
	suffix func() string
}

// New refuses any accelerator type: the adapter rents CPU-only Machines, so a shape that asks
// for an accelerator stops the process at startup instead of a job at Create.
func New(cfg Settings, token string, acceleratorTypes []string, log func(format string, args ...any)) (*Provider, error) {
	if log == nil {
		return nil, errors.New("fly: a log is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if len(acceleratorTypes) > 0 {
		return nil, fmt.Errorf("fly: this adapter rents no accelerators, and a shape asks for %q", acceleratorTypes[0])
	}
	if err := checkToken(token); err != nil {
		return nil, err
	}
	return &Provider{
		log:         log,
		cfg:         cfg,
		token:       token,
		machinesURL: defaultMachinesURL,
		logsURL:     defaultLogsURL,
		http:        &http.Client{Timeout: requestTimeout},
		poll:        pollInterval,
		confirm:     confirmWindow,
		suffix:      randomSuffix,
	}, nil
}

const vendor = "fly"

// architecture is the one the vendor's Machines have.
const architecture = "amd64"

func (p *Provider) Architectures() []string { return []string{architecture} }

// hasStatus tells whether err is the API's answer with one of the given HTTP statuses.
func hasStatus(err error, statuses ...int) bool {
	var failure *vendorapi.StatusError
	if !errors.As(err, &failure) {
		return false
	}
	for _, status := range statuses {
		if failure.Status == status {
			return true
		}
	}
	return false
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
	raw, header, err := vendorapi.Do(p.http, req, vendor, maxAnswer)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return header, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("fly: decode %s %s: %w", method, url, err)
	}
	return header, nil
}
