package nodeimages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var ocispecVersioned = specs.Versioned{SchemaVersion: 2}

// Run against a real containerd: HYPOTHESISLOOP_TEST_CONTAINERD=/run/k3s/containerd/containerd.sock.
func TestALoadedImageIsNamedAfterItsOwnManifestAndUnpacked(t *testing.T) {
	socket := os.Getenv("HYPOTHESISLOOP_TEST_CONTAINERD")
	if socket == "" {
		t.Skip("HYPOTHESISLOOP_TEST_CONTAINERD names no containerd socket")
	}
	ctx := context.Background()
	node, err := Dial(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	image, manifest := ociTar(t, t.Name())
	for range 2 {
		loaded, err := node.Load(ctx, "registry.test/envs/env", bytes.NewReader(image))
		if err != nil || loaded != manifest.String() {
			t.Fatalf("loaded %s, %v, want %s", loaded, err, manifest)
		}
	}
	nctx := namespaces.WithNamespace(ctx, namespace)
	got, err := node.client.ImageService().Get(nctx, "registry.test/envs/env@"+manifest.String())
	if err != nil || got.Target.Digest != manifest || got.Labels[managedLabel] != "managed" {
		t.Fatalf("image %+v, %v, want it named after its manifest and kept by the CRI plugin", got, err)
	}
	unpacked, err := node.client.GetImage(nctx, got.Name)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := unpacked.IsUnpacked(nctx, ""); !ok || err != nil {
		t.Errorf("unpacked %v, %v", ok, err)
	}
	_ = node.client.ImageService().Delete(nctx, got.Name)
}

// ociTar is an OCI layout tar of a one-layer linux/amd64 image whose only file says marker.
func ociTar(t *testing.T, marker string) ([]byte, digest.Digest) {
	t.Helper()
	var layerTar bytes.Buffer
	tw := tar.NewWriter(&layerTar)
	_ = tw.WriteHeader(&tar.Header{Name: "marker", Mode: 0o644, Size: int64(len(marker))})
	_, _ = tw.Write([]byte(marker))
	_ = tw.Close()
	var layer bytes.Buffer
	gz := gzip.NewWriter(&layer)
	_, _ = gz.Write(layerTar.Bytes())
	_ = gz.Close()

	config, _ := json.Marshal(ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromBytes(layerTar.Bytes())}},
	})
	descriptor := func(mediaType string, blob []byte) ocispec.Descriptor {
		return ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(blob), Size: int64(len(blob))}
	}
	manifest, _ := json.Marshal(ocispec.Manifest{
		Versioned: ocispecVersioned,
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    descriptor(ocispec.MediaTypeImageConfig, config),
		Layers:    []ocispec.Descriptor{descriptor(ocispec.MediaTypeImageLayerGzip, layer.Bytes())},
	})
	index, _ := json.Marshal(ocispec.Index{
		Versioned: ocispecVersioned,
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{descriptor(ocispec.MediaTypeImageManifest, manifest)},
	})

	var out bytes.Buffer
	tw = tar.NewWriter(&out)
	write := func(name string, data []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write(data)
	}
	write(ocispec.ImageLayoutFile, []byte(`{"imageLayoutVersion":"1.0.0"}`))
	for _, blob := range [][]byte{layer.Bytes(), config, manifest} {
		write("blobs/sha256/"+digest.FromBytes(blob).Encoded(), blob)
	}
	write(ocispec.ImageIndexFile, index)
	_ = tw.Close()
	return out.Bytes(), digest.FromBytes(manifest)
}
