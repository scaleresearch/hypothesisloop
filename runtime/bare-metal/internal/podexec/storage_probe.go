package podexec

import (
	"bytes"
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
	defer engine.ImageRemove(ctx, image, client.ImageRemoveOptions{Force: true})

	create := func(storageOpt map[string]string) error {
		created, err := engine.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config:     &container.Config{Image: image, Cmd: []string{"/never-started"}},
			HostConfig: &container.HostConfig{StorageOpt: storageOpt},
		})
		if err != nil {
			return err
		}
		if _, err := engine.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("remove the probe container %s: %w", created.ID, err)
		}
		return nil
	}
	if err := create(nil); err != nil {
		return false, "", fmt.Errorf("podexec: storage probe: the engine does not create a container: %w", err)
	}
	if err := create(map[string]string{"size": probeSize}); err != nil {
		return false, err.Error(), nil
	}
	return true, "", nil
}
