// Package imagedigest names an image by the digest of the manifest its reference names now, so
// what is keyed on an image changes when its content does and never with its tag.
package imagedigest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/distribution/reference"
)

// Resolver pins references to digests, asking a reference's registry at most once per ttl.
type Resolver struct {
	ttl     time.Duration
	resolve func(ctx context.Context, ref string, plainHTTP bool) (string, error)

	mu     sync.Mutex
	pinned map[string]pin
}

type pin struct {
	digest string
	at     time.Time
}

// New asks registries anonymously, as a node that pulls a job image does.
func New(ttl time.Duration) *Resolver {
	client := &http.Client{Timeout: 15 * time.Second}
	return &Resolver{ttl: ttl, pinned: map[string]pin{}, resolve: func(ctx context.Context, ref string, plainHTTP bool) (string, error) {
		_, desc, err := docker.NewResolver(docker.ResolverOptions{Client: client, PlainHTTP: plainHTTP}).Resolve(ctx, ref)
		if err != nil {
			return "", err
		}
		return desc.Digest.String(), nil
	}}
}

// Pinned is ref as repository@digest: one that names its digest already keeps it, and any other
// takes the digest its registry answers for it. A registry on an explicit port that does not
// answer over TLS is asked over plain HTTP, as a local registry is served.
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
	r.mu.Lock()
	cached, ok := r.pinned[tagged]
	r.mu.Unlock()
	if ok && time.Since(cached.at) < r.ttl {
		return repository + "@" + cached.digest, nil
	}
	digest, err := r.resolve(ctx, tagged, false)
	if _, port, splitErr := net.SplitHostPort(reference.Domain(named)); err != nil && splitErr == nil && port != "" {
		if plain, plainErr := r.resolve(ctx, tagged, true); plainErr == nil {
			digest, err = plain, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("imagedigest: %s: %w", tagged, err)
	}
	r.mu.Lock()
	r.pinned[tagged] = pin{digest: digest, at: time.Now()}
	r.mu.Unlock()
	return repository + "@" + digest, nil
}
