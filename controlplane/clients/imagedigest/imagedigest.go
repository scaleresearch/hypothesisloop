package imagedigest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// maxManifestBytes bounds a manifest or an image config read.
const maxManifestBytes = 4 << 20

// Image is what a registry says of a reference: the image it names, repository@digest, and the
// linux architectures it runs on.
type Image struct {
	Pinned        string
	Architectures []string
}

// Resolver reads images from their registries, asking every time.
type Resolver struct {
	client    *http.Client
	plainHTTP map[string]bool
}

// New asks registries over plain HTTP for the hosts plainHTTP names by host[:port], and over TLS
// for every other.
func New(plainHTTP ...string) *Resolver {
	hosts := make(map[string]bool, len(plainHTTP))
	for _, host := range plainHTTP {
		hosts[host] = true
	}
	transport := &http.Transport{DialContext: dial(hosts), TLSHandshakeTimeout: 10 * time.Second}
	return &Resolver{client: &http.Client{Timeout: 30 * time.Second, Transport: transport}, plainHTTP: hosts}
}

// dial reaches the plainHTTP hosts wherever they are and every other address only on the public
// internet: a ref, its registry's redirects and the token realm it names are the customer's, and
// would otherwise have the control plane send requests into its own network.
func dial(plainHTTP map[string]bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	open := &net.Dialer{Timeout: 10 * time.Second}
	public := &net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if plainHTTP[addr] || plainHTTP[host] {
			return open.DialContext(ctx, network, addr)
		}
		return public.DialContext(ctx, network, addr)
	}
}

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

func publicOnly(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	ip := addrPort.Addr().Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || sharedAddressSpace.Contains(ip) {
		return fmt.Errorf("imagedigest: %s is not a public address", ip)
	}
	return nil
}

// Resolve reads ref, a tag or a digest, with login for its registry host, or anonymously when
// login is nil.
func (r *Resolver) Resolve(ctx context.Context, ref string, login *domain.RegistryLogin) (Image, error) {
	named, desc, fetcher, err := r.remote(ctx, ref, login)
	if err != nil {
		return Image{}, err
	}
	architectures, err := architectures(ctx, fetcher, desc)
	if err != nil {
		return Image{}, fmt.Errorf("imagedigest: %s: %w", named, err)
	}
	return Image{Pinned: named.Name() + "@" + desc.Digest.String(), Architectures: architectures}, nil
}

// Config reads the config of the linux image of architecture that ref names, with login as
// Resolve takes it.
func (r *Resolver) Config(ctx context.Context, ref, architecture string, login *domain.RegistryLogin) (ocispec.ImageConfig, error) {
	named, desc, fetcher, err := r.remote(ctx, ref, login)
	if err != nil {
		return ocispec.ImageConfig{}, err
	}
	if images.IsIndexType(desc.MediaType) {
		var index ocispec.Index
		if err := fetchJSON(ctx, fetcher, desc, &index); err != nil {
			return ocispec.ImageConfig{}, fmt.Errorf("imagedigest: %s: %w", named, err)
		}
		i := slices.IndexFunc(index.Manifests, func(m ocispec.Descriptor) bool {
			return m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == architecture
		})
		if i < 0 {
			return ocispec.ImageConfig{}, fmt.Errorf("imagedigest: %s has no linux/%s image", named, architecture)
		}
		desc = index.Manifests[i]
	}
	var manifest ocispec.Manifest
	if err := fetchJSON(ctx, fetcher, desc, &manifest); err != nil {
		return ocispec.ImageConfig{}, fmt.Errorf("imagedigest: %s: %w", named, err)
	}
	var config ocispec.Image
	if err := fetchJSON(ctx, fetcher, manifest.Config, &config); err != nil {
		return ocispec.ImageConfig{}, fmt.Errorf("imagedigest: %s: %w", named, err)
	}
	if config.OS != "linux" || config.Architecture != architecture {
		return ocispec.ImageConfig{}, fmt.Errorf("imagedigest: %s runs on %s/%s, not linux/%s", named, config.OS, config.Architecture, architecture)
	}
	return config.Config, nil
}

// remote is what ref names in its registry and what reads from that registry.
func (r *Resolver) remote(ctx context.Context, ref string, login *domain.RegistryLogin) (reference.Named, ocispec.Descriptor, remotes.Fetcher, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return nil, ocispec.Descriptor{}, nil, fmt.Errorf("imagedigest: %q: %w", ref, err)
	}
	named = reference.TagNameOnly(named)
	credentials := func(host string) (string, string, error) {
		if login != nil && host == login.Registry {
			return login.Username, login.Password, nil
		}
		return "", "", nil
	}
	resolver := docker.NewResolver(docker.ResolverOptions{Hosts: docker.ConfigureDefaultRegistries(
		docker.WithClient(r.client),
		docker.WithPlainHTTP(func(host string) (bool, error) { return r.plainHTTP[host], nil }),
		docker.WithAuthorizer(docker.NewDockerAuthorizer(docker.WithAuthClient(r.client), docker.WithAuthCreds(credentials))),
	)})
	name, desc, err := resolver.Resolve(ctx, named.String())
	if err != nil {
		return nil, ocispec.Descriptor{}, nil, fmt.Errorf("imagedigest: %s: %w", named, err)
	}
	fetcher, err := resolver.Fetcher(ctx, name)
	if err != nil {
		return nil, ocispec.Descriptor{}, nil, fmt.Errorf("imagedigest: %s: %w", named, err)
	}
	return named, desc, fetcher, nil
}

// architectures is the linux architectures the image desc names runs on: those of its index, or
// the one of its config.
func architectures(ctx context.Context, fetcher remotes.Fetcher, desc ocispec.Descriptor) ([]string, error) {
	if images.IsIndexType(desc.MediaType) {
		var index ocispec.Index
		if err := fetchJSON(ctx, fetcher, desc, &index); err != nil {
			return nil, err
		}
		var out []string
		for _, m := range index.Manifests {
			if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture != "unknown" && !slices.Contains(out, m.Platform.Architecture) {
				out = append(out, m.Platform.Architecture)
			}
		}
		slices.Sort(out)
		return out, nil
	}
	if !images.IsManifestType(desc.MediaType) {
		return nil, fmt.Errorf("names a %s, which is no image", desc.MediaType)
	}
	var manifest ocispec.Manifest
	if err := fetchJSON(ctx, fetcher, desc, &manifest); err != nil {
		return nil, err
	}
	var config ocispec.Image
	if err := fetchJSON(ctx, fetcher, manifest.Config, &config); err != nil {
		return nil, err
	}
	if config.OS != "linux" || config.Architecture == "" {
		return nil, nil
	}
	return []string{config.Architecture}, nil
}

func fetchJSON(ctx context.Context, fetcher remotes.Fetcher, desc ocispec.Descriptor, into any) error {
	body, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		return err
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, maxManifestBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}
