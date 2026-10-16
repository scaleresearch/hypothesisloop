// Package imagedigest names an image by the digest of the manifest its reference names now, so
// what is keyed on an image changes when its content does and never with its tag.
package imagedigest

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/distribution/reference"
)

// Resolver pins references to digests, asking a reference's registry every time.
type Resolver struct {
	resolve   func(ctx context.Context, ref string, plainHTTP bool) (string, error)
	plainHTTP map[string]bool
}

// New asks registries anonymously, as a node that pulls a job image does: over plain HTTP the
// registries plainHTTP names by host[:port], and over TLS every other.
func New(plainHTTP ...string) *Resolver {
	client := &http.Client{Timeout: 15 * time.Second}
	hosts := make(map[string]bool, len(plainHTTP))
	for _, host := range plainHTTP {
		hosts[host] = true
	}
	return &Resolver{plainHTTP: hosts, resolve: func(ctx context.Context, ref string, plainHTTP bool) (string, error) {
		_, desc, err := docker.NewResolver(docker.ResolverOptions{Client: client, PlainHTTP: plainHTTP}).Resolve(ctx, ref)
		if err != nil {
			return "", err
		}
		return desc.Digest.String(), nil
	}}
}

// Pinned is ref as repository@digest: one that names its digest already keeps it, and any other
// takes the digest its registry answers for it.
func (r *Resolver) Pinned(ctx context.Context, ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("imagedigest: %q: %w", ref, err)
	}
	repository := named.Name()
	if digested, ok := named.(reference.Digested); ok {
		return repository + "@" + digested.Digest().String(), nil
	}
	tagged := reference.TagNameOnly(named).String()
	digest, err := r.resolve(ctx, tagged, r.plainHTTP[reference.Domain(named)])
	if err != nil {
		return "", fmt.Errorf("imagedigest: %s: %w", tagged, err)
	}
	return repository + "@" + digest, nil
}
