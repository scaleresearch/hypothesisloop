// Package nodeimages loads the environment images this cluster builds into the containerd of the
// node the cluster-agent runs on, where the kubelet finds them, so a job placed on that node
// starts without pulling back what the builder just pushed. A job on any other node pulls as
// before: the job names its image repository@digest, and an image is only ever named after the
// digest of its own manifest.
package nodeimages

import (
	"context"
	"fmt"
	"io"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/go-digest"
)

// namespace is the containerd namespace the kubelet's images are in.
const namespace = "k8s.io"

// managedLabel marks an image as one the CRI plugin keeps, as it marks the images it pulls.
const managedLabel = "io.cri-containerd.image"

// Containerd is the image store of one node.
type Containerd struct {
	client      *containerd.Client
	snapshotter string
}

// Dial reaches the containerd at address, a unix socket. snapshotter is the one the kubelet's
// containers are made with; empty is containerd's default.
func Dial(ctx context.Context, address, snapshotter string) (*Containerd, error) {
	client, err := containerd.New(address, containerd.WithDefaultNamespace(namespace))
	if err != nil {
		return nil, fmt.Errorf("nodeimages: %s: %w", address, err)
	}
	if _, err := client.Version(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("nodeimages: %s: %w", address, err)
	}
	return &Containerd{client: client, snapshotter: snapshotter}, nil
}

// Load imports the OCI layout tar image as repository@<manifest digest> and unpacks it. It reads
// the tar to its end before it unpacks, since the push waits for the tar.
func (c *Containerd) Load(ctx context.Context, repository string, image io.Reader) (string, error) {
	ctx = namespaces.WithNamespace(ctx, namespace)
	imported, err := c.client.Import(ctx, image,
		containerd.WithDigestRef(func(d digest.Digest) string { return repository + "@" + d.String() }),
		containerd.WithImageLabels(map[string]string{managedLabel: "managed"}))
	if _, drain := io.Copy(io.Discard, image); err == nil {
		err = drain
	}
	if err != nil {
		return "", fmt.Errorf("import: %w", err)
	}
	if len(imported) != 1 {
		return "", fmt.Errorf("import: %d images for this node's platform, want one", len(imported))
	}
	loaded := imported[0].Target.Digest.String()
	if err := containerd.NewImage(c.client, imported[0]).Unpack(ctx, c.snapshotter); err != nil {
		return loaded, fmt.Errorf("unpack %s: %w", loaded, err)
	}
	return loaded, nil
}
