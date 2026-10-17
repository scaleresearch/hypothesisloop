package imagedigest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// registry serves repository model's tags and the manifests and blobs they lead to, by digest,
// over plain HTTP, as the distribution API does. With a login set it answers only a request
// that carries it.
type registry struct {
	tags     map[string]ocispec.Descriptor
	content  map[digest.Digest][]byte
	user     string
	password string
}

func newRegistry() *registry {
	return &registry{tags: map[string]ocispec.Descriptor{}, content: map[digest.Digest][]byte{}}
}

func (r *registry) put(mediaType string, v any) ocispec.Descriptor {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	d := digest.FromBytes(raw)
	r.content[d] = raw
	return ocispec.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(raw))}
}

// image is a single manifest whose config runs on os/arch.
func (r *registry) image(os, arch string) ocispec.Descriptor {
	config := r.put(ocispec.MediaTypeImageConfig, ocispec.Image{Platform: ocispec.Platform{OS: os, Architecture: arch}})
	return r.put(ocispec.MediaTypeImageManifest, ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: config, Layers: []ocispec.Descriptor{}})
}

func (r *registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.user != "" {
		if user, password, ok := req.BasicAuth(); !ok || user != r.user || password != r.password {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if req.URL.Path == "/v2/" {
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/team/model/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	kind, ref, _ := strings.Cut(rest, "/")
	var desc ocispec.Descriptor
	if d, err := digest.Parse(ref); err == nil {
		raw, ok := r.content[d]
		if !ok {
			http.NotFound(w, req)
			return
		}
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(raw, &probe)
		desc = ocispec.Descriptor{MediaType: probe.MediaType, Digest: d, Size: int64(len(raw))}
		if kind == "blobs" {
			desc.MediaType = "application/octet-stream"
		}
	} else if desc, ok = r.tags[ref]; !ok || kind != "manifests" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", desc.MediaType)
	w.Header().Set("Docker-Content-Digest", desc.Digest.String())
	w.Header().Set("Content-Length", strconv.FormatInt(desc.Size, 10))
	if req.Method == http.MethodGet {
		_, _ = w.Write(r.content[desc.Digest])
	}
}

func serve(t *testing.T, r *registry) (host string, resolver *Resolver) {
	t.Helper()
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	host = strings.TrimPrefix(server.URL, "http://")
	return host, New(host)
}

// An index names every linux architecture it holds once and is pinned by its own digest; an
// attestation manifest and another OS's are none of them.
func TestAnIndexRunsOnEachLinuxArchitectureItHolds(t *testing.T) {
	r := newRegistry()
	platform := func(desc ocispec.Descriptor, os, arch string) ocispec.Descriptor {
		desc.Platform = &ocispec.Platform{OS: os, Architecture: arch}
		return desc
	}
	index := r.put(ocispec.MediaTypeImageIndex, ocispec.Index{MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{
		platform(r.image("linux", "arm64"), "linux", "arm64"),
		platform(r.image("linux", "amd64"), "linux", "amd64"),
		platform(r.image("linux", "amd64"), "linux", "amd64"),
		platform(r.image("windows", "amd64"), "windows", "arm"),
		platform(r.image("unknown", "unknown"), "unknown", "unknown"),
	}})
	r.tags["v1"] = index
	host, resolver := serve(t, r)

	got, err := resolver.Resolve(context.Background(), host+"/team/model:v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pinned != host+"/team/model@"+index.Digest.String() || !slices.Equal(got.Architectures, []string{"amd64", "arm64"}) {
		t.Fatalf("resolved %+v, want the index's digest running on amd64 and arm64", got)
	}
}

// A single manifest runs on the architecture of its config, and one of another OS on none.
func TestASingleManifestRunsOnTheArchitectureOfItsConfig(t *testing.T) {
	r := newRegistry()
	arm := r.image("linux", "arm64")
	r.tags["arm"], r.tags["windows"] = arm, r.image("windows", "amd64")
	host, resolver := serve(t, r)

	got, err := resolver.Resolve(context.Background(), host+"/team/model:arm", nil)
	if err != nil || got.Pinned != host+"/team/model@"+arm.Digest.String() || !slices.Equal(got.Architectures, []string{"arm64"}) {
		t.Fatalf("resolved %+v, %v, want the manifest's digest running on arm64", got, err)
	}
	// A digest is read as it is, and names the same image.
	byDigest, err := resolver.Resolve(context.Background(), host+"/team/model@"+arm.Digest.String(), nil)
	if err != nil || byDigest.Pinned != got.Pinned {
		t.Fatalf("by digest: %+v, %v", byDigest, err)
	}
	if windows, err := resolver.Resolve(context.Background(), host+"/team/model:windows", nil); err != nil || len(windows.Architectures) != 0 {
		t.Fatalf("a windows image: %+v, %v, want no linux architecture", windows, err)
	}
	if _, err := resolver.Resolve(context.Background(), host+"/team/model:missing", nil); err == nil {
		t.Fatal("a tag the registry lacks resolved")
	}
}

// A registry that takes a login is read with the one given for its host and refuses a read with none.
func TestAnImageIsReadWithTheLoginOfItsRegistry(t *testing.T) {
	r := newRegistry()
	r.user, r.password = "oauth2accesstoken", "pull-token"
	r.tags["v1"] = r.image("linux", "amd64")
	host, resolver := serve(t, r)

	if _, err := resolver.Resolve(context.Background(), host+"/team/model:v1", nil); err == nil {
		t.Fatal("read a private image without a login")
	}
	if _, err := resolver.Resolve(context.Background(), host+"/team/model:v1", &domain.RegistryLogin{Registry: "other.example", Username: r.user, Password: r.password}); err == nil {
		t.Fatal("sent the login of another registry")
	}
	got, err := resolver.Resolve(context.Background(), host+"/team/model:v1", &domain.RegistryLogin{Registry: host, Username: r.user, Password: r.password})
	if err != nil || !slices.Equal(got.Architectures, []string{"amd64"}) {
		t.Fatalf("with its login: %+v, %v", got, err)
	}
}
