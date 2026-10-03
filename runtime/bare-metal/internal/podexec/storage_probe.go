package podexec

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// storageProber is the part of the Engine API the storage probe calls.
type storageProber interface {
	ImageImport(ctx context.Context, source client.ImageImportSource, ref string, options client.ImageImportOptions) (client.ImageImportResult, error)
	ImageRemove(ctx context.Context, imageID string, options client.ImageRemoveOptions) (client.ImageRemoveResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// probeSize is a storage size written as a job's is (BuildContainerSpec): in bytes.
const probeSize = "1073741824"

// probeStorageQuota asks the engine whether it holds a container to a storage size, the only
// one who knows: it depends on the engine's driver, the filesystem of its store, that
// filesystem's mount options and what the engine may do to it. Two containers are created from
// an empty image and never started. The first, without a size, shows the engine creates
// containers at all, so a refusal of the second is about the size. why is the engine's own
// words when it refuses. Both containers and the image are removed again.
//
// An engine that takes the size has not said it holds to it. Docker with the containerd image
// store takes any size and ignores it: a container given 1 GiB there wrote past it. That store
// shows in the container the engine made, whose filesystem is a containerd snapshot and has no
// graph driver, the one part of an engine that holds a container to a size.
func probeStorageQuota(ctx context.Context, engine storageProber) (enforced bool, why string, err error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return false, "", err
	}
	image := "hypothesisloop-storage-probe:" + hex.EncodeToString(suffix)

	// 1024 zero bytes are a tar archive with nothing in it.
	imported, err := engine.ImageImport(ctx, client.ImageImportSource{Source: bytes.NewReader(make([]byte, 1024)), SourceName: "-"}, image, client.ImageImportOptions{})
	if err != nil {
		return false, "", fmt.Errorf("podexec: storage probe: import an empty image: %w", err)
	}
	answer, err := io.ReadAll(imported)
	imported.Close()
	if err != nil {
		return false, "", fmt.Errorf("podexec: storage probe: import an empty image: %w", err)
	}
	for _, line := range bytes.Split(answer, []byte("\n")) {
		var said struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(line, &said) == nil && said.Error != "" {
			return false, "", fmt.Errorf("podexec: storage probe: import an empty image: %s", said.Error)
		}
	}
	// An image that stays is one more on the node with every start of the agent.
	defer func() {
		if _, removeErr := engine.ImageRemove(ctx, image, client.ImageRemoveOptions{Force: true}); removeErr != nil && err == nil {
			enforced, why, err = false, "", fmt.Errorf("podexec: storage probe: remove the probe image %s: %w", image, removeErr)
		}
	}()

	create := func(storageOpt map[string]string) (made container.InspectResponse, refusal, err error) {
		created, err := engine.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config:     &container.Config{Image: image, Cmd: []string{"/never-started"}},
			HostConfig: &container.HostConfig{StorageOpt: storageOpt},
		})
		if err != nil {
			return made, err, nil
		}
		inspected, inspectErr := engine.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
		if _, err := engine.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			return made, nil, fmt.Errorf("remove the probe container %s: %w", created.ID, err)
		}
		if inspectErr != nil {
			return made, nil, fmt.Errorf("inspect the probe container %s: %w", created.ID, inspectErr)
		}
		return inspected.Container, nil, nil
	}
	if _, refusal, err := create(nil); refusal != nil || err != nil {
		return false, "", fmt.Errorf("podexec: storage probe: the engine does not create a container: %w", cmp.Or(refusal, err))
	}
	made, refusal, err := create(map[string]string{"size": probeSize})
	if err != nil {
		return false, "", fmt.Errorf("podexec: storage probe: %w", err)
	}
	if refusal != nil {
		return false, refusal.Error(), nil
	}
	if made.GraphDriver == nil {
		return false, "the engine keeps containers in containerd snapshots, which take a storage size and ignore it", nil
	}
	return true, "", nil
}
