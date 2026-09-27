package podexec

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/storage"
	"github.com/moby/moby/client"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// probedEngine creates containers unless told to refuse a size or every create, and keeps count
// of what it holds.
type probedEngine struct {
	refusesSize, refusesAll bool
	// containerdStore is Docker with the containerd image store, whose containers have no graph driver.
	containerdStore    bool
	images, containers map[string]bool
	sizes              []string
}

func (e *probedEngine) ImageImport(_ context.Context, _ client.ImageImportSource, ref string, _ client.ImageImportOptions) (client.ImageImportResult, error) {
	e.images[ref] = true
	return io.NopCloser(strings.NewReader(`{"status":"sha256:abc"}` + "\n")), nil
}

func (e *probedEngine) ImageRemove(_ context.Context, ref string, _ client.ImageRemoveOptions) (client.ImageRemoveResult, error) {
	delete(e.images, ref)
	return client.ImageRemoveResult{}, nil
}

func (e *probedEngine) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	size := options.HostConfig.StorageOpt["size"]
	e.sizes = append(e.sizes, size)
	if e.refusesAll {
		return client.ContainerCreateResult{}, errors.New("no space left on device")
	}
	if size != "" && e.refusesSize {
		return client.ContainerCreateResult{}, errors.New("--storage-opt is supported only for overlay over xfs with 'pquota' mount option")
	}
	id := "probe-" + size
	e.containers[id] = true
	return client.ContainerCreateResult{ID: id}, nil
}

func (e *probedEngine) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if !e.containers[id] {
		return client.ContainerInspectResult{}, errors.New("no such container")
	}
	made := container.InspectResponse{ID: id, HostConfig: &container.HostConfig{StorageOpt: map[string]string{"size": strings.TrimPrefix(id, "probe-")}}}
	if !e.containerdStore {
		made.GraphDriver = &storage.DriverData{Name: "overlay2"}
	}
	return client.ContainerInspectResult{Container: made}, nil
}

func (e *probedEngine) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	delete(e.containers, id)
	return client.ContainerRemoveResult{}, nil
}

// Whether a job is held to its storage request is the engine's to say. Read off the store's
// filesystem and mount options, it was reported as enforced on an engine that then refused
// every container given a size. Read off a create that succeeds, it was reported as enforced on
// an engine that takes the size and ignores it.
func TestStorageIsEnforcedOnlyWhenTheEngineTakesAContainerWithASize(t *testing.T) {
	for name, tc := range map[string]struct {
		engine   probedEngine
		enforced bool
		why      string
		fails    bool
	}{
		"the engine takes a size":   {probedEngine{}, true, "", false},
		"the engine refuses a size": {probedEngine{refusesSize: true}, false, "supported only for overlay over xfs", false},
		// Docker 29 on box-b took a 1 GiB size, recorded it, and gave the container the whole disk.
		"the engine takes a size and ignores it": {probedEngine{containerdStore: true}, false, "containerd snapshots", false},
		"the engine creates no container":        {probedEngine{refusesAll: true}, false, "", true},
	} {
		engine := tc.engine
		engine.images, engine.containers = map[string]bool{}, map[string]bool{}
		enforced, why, err := probeStorageQuota(context.Background(), &engine)
		if (err != nil) != tc.fails {
			t.Errorf("%s: err = %v", name, err)
		}
		if enforced != tc.enforced || !strings.Contains(why, tc.why) {
			t.Errorf("%s: enforced = %v, why = %q", name, enforced, why)
		}
		if len(engine.images) != 0 || len(engine.containers) != 0 {
			t.Errorf("%s: the probe left images %v and containers %v on the engine", name, engine.images, engine.containers)
		}
		if !tc.fails && (len(engine.sizes) != 2 || engine.sizes[0] != "" || engine.sizes[1] != probeSize) {
			t.Errorf("%s: containers were created with sizes %q, want one without and one with a size", name, engine.sizes)
		}
	}
}

func sizedExperiment() *domain.Experiment {
	return &domain.Experiment{
		Data: testDataAccess(), Token: "test-job-token",
		ID: "sized", AgentID: "agent", ProjectID: "project",
		EstimatedDurationHours: 0.02, CapacityTier: domain.CapacityGuaranteed,
		Job: domain.JobSpec{
			Image: "example.invalid/workload", CPU: "1", Memory: "1Gi", Storage: "128Mi",
			MaxRetries: maxRetriesPtr(0), TerminationGracePeriodSeconds: int64Ptr(5),
		},
	}
}

// podman reads no binary suffix in a size and refused every job given "128Mi", so a job's size
// goes to the engine in bytes, and only where the engine was found to take one.
func TestAJobIsGivenItsStorageSizeInBytesAndOnlyWhereTheEngineEnforcesIt(t *testing.T) {
	exp := sizedExperiment()
	for enforced, want := range map[bool]string{true: "134217728", false: ""} {
		e := &Executor{apiURL: "http://example.invalid:8083", storageQuotaEnforced: enforced}
		cs, err := e.BuildContainerSpec(exp, Placement{})
		if err != nil {
			t.Fatal(err)
		}
		if cs.StorageOptSize != want {
			t.Errorf("enforced = %v: size handed to the engine = %q, want %q", enforced, cs.StorageOptSize, want)
		}
	}
}

// A node's storage is what its jobs can write, and they write to the engine's store. An engine
// elsewhere has a store this host cannot measure, and saying so beats measuring another disk.
func TestStorageIsMeasuredOnTheEnginesStoreOrNotAtAll(t *testing.T) {
	store := t.TempDir()
	if dir, err := engineStore(store); err != nil || dir != store {
		t.Fatalf("a store on this host: dir = %q, err = %v", dir, err)
	}
	if _, err := engineStore(store + "/var/lib/containers/storage"); err == nil || !strings.Contains(err.Error(), "SCRATCH_DIR") {
		t.Fatalf("a store that is not on this host: err = %v, want it to name the setting that replaces it", err)
	}
}

// A container runs in the image its job's desired state names. A job that reaches the node with
// none is not started: the control plane decides the image, and the node has none to put in.
func TestAContainerRunsInExactlyTheImageDesiredStateNames(t *testing.T) {
	e := &Executor{apiURL: "http://example.invalid:8083"}
	if e.OnlyImage() != "" {
		t.Fatalf("the node reports it runs only %q, want any image", e.OnlyImage())
	}
	named := sizedExperiment()
	cs, err := e.BuildContainerSpec(named, Placement{})
	if err != nil {
		t.Fatal(err)
	}
	if cs.Image != named.Job.Image {
		t.Errorf("the container runs in %q, want %q", cs.Image, named.Job.Image)
	}
	unnamed := sizedExperiment()
	unnamed.Job.Image = ""
	if _, err := e.BuildContainerSpec(unnamed, Placement{}); err == nil {
		t.Error("a job with no image in desired state was built")
	}
}
