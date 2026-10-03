// Package providertest is the conformance suite of the provider contract. An adapter runs it
// against its real account before it may be configured: every check rents real instances and
// reads the contract's sentences back from what the vendor did.
package providertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/runtime/cloud/internal/provider"
)

type Options struct {
	// Template carries the image and the size of the instances to rent. The image must provide
	// sh, sleep and a wget that speaks TLS. The suite sets Name, Command and Env.
	Template provider.Spec
	// OutboundURL is fetched from inside an instance over HTTPS.
	OutboundURL string
	// StartTimeout bounds Create returning to the command's first output.
	StartTimeout time.Duration
	// SettleTimeout bounds a command ending to the instance showing as exited.
	SettleTimeout time.Duration
}

const (
	pollInterval = 5 * time.Second
	cleanupLimit = 2 * time.Minute
	logTail      = 200
)

// Run rents three small instances side by side and destroys them however it ends (see guard).
// Each subtest is one part of the contract, so a vendor's result reads check by check.
func Run(t *testing.T, p provider.Provider, opts Options) {
	opts.validate(t)
	p = &paced{Provider: p}
	ctx := guard(t, p)
	checks := map[string]func(*testing.T, *run){
		"Lifecycle":                lifecycle,
		"CommandThatEnds":          commandThatEnds,
		"StopLetsTheCommandFinish": stopLetsTheCommandFinish,
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			check(t, newRun(t, ctx, p, opts))
		})
	}
}

func (o Options) validate(t *testing.T) {
	t.Helper()
	if o.Template.Image == "" || o.OutboundURL == "" || o.StartTimeout <= 0 || o.SettleTimeout <= 0 {
		t.Fatalf("providertest: Template.Image, OutboundURL, StartTimeout and SettleTimeout are required, got %+v", o)
	}
}

// run is one check's own corner of the account: every instance it rents carries its prefix, and
// everything under the prefix is destroyed when the check ends.
type run struct {
	ctx    context.Context
	p      provider.Provider
	opts   Options
	prefix string
	// ran is the instances whose command has been seen running or exited (checkState).
	ran map[string]bool
}

func newRun(t *testing.T, ctx context.Context, p provider.Provider, opts Options) *run {
	t.Helper()
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random prefix: %v", err)
	}
	r := &run{ctx: ctx, p: p, opts: opts, prefix: suitePrefix + hex.EncodeToString(raw) + "-", ran: map[string]bool{}}
	t.Cleanup(func() { r.destroyAll(t) })
	return r
}

func (r *run) destroyAll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupLimit)
	defer cancel()
	left, err := r.p.List(ctx, r.prefix)
	if err != nil {
		t.Errorf("cleanup: List(%q) failed, instances may still be rented: %v", r.prefix, err)
		return
	}
	for _, i := range left {
		if err := r.p.Destroy(ctx, i.ID); err != nil {
			t.Errorf("cleanup: instance %s (%s) is still rented: %v", i.ID, i.Name, err)
		}
	}
}

func (r *run) spec(name string, script string, args ...string) provider.Spec {
	spec := r.opts.Template
	spec.Name = r.prefix + name
	spec.Command = append([]string{"sh", "-c", script, "conformance"}, args...)
	return spec
}

func (r *run) create(t *testing.T, ctx context.Context, spec provider.Spec) (string, time.Time) {
	t.Helper()
	id, err := r.p.Create(ctx, spec)
	asked := time.Now()
	if err != nil {
		t.Fatalf("Create(%s): %v", spec.Name, err)
	}
	if id == "" {
		t.Fatalf("Create(%s) returned no id", spec.Name)
	}
	return id, asked
}

func since(start time.Time) time.Duration {
	return time.Since(start).Round(time.Second)
}
