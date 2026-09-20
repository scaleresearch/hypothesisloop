package podexec

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/moby/moby/client"

	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

// Podman/Docker container labels have no k8s-style value restrictions, so every
// workloadkeys.* key this runtime uses is a plain label (unlike k8sexec, which puts a few on
// annotations instead).
const (
	LabelManagedBy       = workloadkeys.ManagedBy
	LabelExperimentID    = workloadkeys.ExperimentID
	LabelAgentID         = workloadkeys.AgentID
	LabelCapacityTier    = workloadkeys.CapacityTier
	LabelAcceleratorType = workloadkeys.AcceleratorType
	LabelAcceleratorCnt  = workloadkeys.AcceleratorCount
	LabelDesiredSpecHash = workloadkeys.DesiredSpecHash
	LabelCPUCores        = workloadkeys.CPUCoresMilli
	LabelMemoryBytes     = workloadkeys.MemoryBytes
	LabelStorageBytes    = workloadkeys.StorageBytes
	LabelGraceSeconds    = workloadkeys.GraceSeconds
	LabelAttempt         = workloadkeys.Attempt
	LabelCheckpointGrace = workloadkeys.CheckpointGraceSeconds

	ManagedByValue = workloadkeys.ManagedByValue

	// containerNamePrefix keeps this runtime's containers trivially greppable/distinguishable
	// from anything else podman might be running on the node.
	containerNamePrefix = "hypothesisloop-"
)

// Config mirrors k8sexec.Config's shape: every operational value is required explicitly, no
// implicit defaults.
type Config struct {
	APIURL string
	// ScratchDir is the filesystem this node's Storage quota is measured/enforced against
	// (podman --storage-opt size=, statfs'd for total/available). Required.
	ScratchDir string
	// NodeLabels are this node's static facts (zone, disk class, ...) merged with
	// probe-derived accelerator labels in GetLiveAcceleratorCapacitySnapshot, the bare-node analogue
	// of a single fleet.yaml node entry (different-backend.md §4.2).
	NodeLabels map[string]string
	// NodeName overrides the OS hostname as this node's reported identity. Optional.
	NodeName string
}

type Executor struct {
	apiURL     string
	scratchDir string
	nodeLabels map[string]string
	// nodeName is this node's own identity, reported as the sole entry in every
	// cluster->node->... map the Backend contract expects (a bare-node "cluster" is one node).
	nodeName string
	// clusterID is the host's machine id, read once at startup: set at OS install and stable
	// for the machine's life.
	clusterID string
	// docker is the Engine API client connected to whichever engine resolveEngineClient found
	// (podman's Docker-compatible socket, or dockerd's own) — every container operation this
	// package makes goes through it; nothing here shells out.
	docker engine
	// engineName is the resolved engine's name ("podman" or "docker"), for logging only.
	engineName string
	// storageQuotaEnforced is decided once at startup (see scratchQuotaSupported): whether
	// ScratchDir's filesystem can back a per-container storage-opt size quota. When false,
	// Storage is still a required, recorded, accounted-for field (capacity math still treats it
	// as a hard dimension) but is not passed to the engine as an enforced quota — see
	// BuildContainerSpec.
	storageQuotaEnforced bool
	probe                func(context.Context) ([]acceleratorDevice, error)

	mu       sync.Mutex
	inFlight map[string]operation
	workers  sync.WaitGroup
}

// engine is the part of the Engine API client this package calls.
type engine interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
}

func New(cfg Config) (*Executor, error) {
	if cfg.APIURL == "" {
		return nil, fmt.Errorf("podexec: APIURL is required")
	}
	if cfg.ScratchDir == "" {
		return nil, fmt.Errorf("podexec: ScratchDir is required")
	}
	clusterID, err := readClusterID(machineIDPath)
	if err != nil {
		return nil, err
	}
	docker, engineName, err := resolveEngineClient(context.Background())
	if err != nil {
		return nil, err
	}
	nodeName, err := localNodeName()
	if err != nil {
		return nil, fmt.Errorf("podexec: resolve node name: %w", err)
	}
	if cfg.NodeName != "" {
		nodeName = cfg.NodeName
	}
	labels := make(map[string]string, len(cfg.NodeLabels))
	for k, v := range cfg.NodeLabels {
		labels[k] = v
	}
	quotaSupported, err := scratchQuotaSupported(cfg.ScratchDir)
	if err != nil {
		return nil, err
	}
	return &Executor{
		apiURL:               cfg.APIURL,
		scratchDir:           cfg.ScratchDir,
		nodeLabels:           labels,
		nodeName:             nodeName,
		clusterID:            clusterID,
		docker:               docker,
		engineName:           engineName,
		storageQuotaEnforced: quotaSupported,
		probe:                probeAccelerators,
	}, nil
}

// StorageQuotaEnforced reports whether this node can enforce per-container storage quotas
// (requires an XFS-backed overlay store) — surfaced so main.go can log it plainly at startup.
func (e *Executor) StorageQuotaEnforced() bool { return e.storageQuotaEnforced }

// NodeName returns this executor's node identity, for callers (main) that log it.
func (e *Executor) NodeName() string { return e.nodeName }

// Engine returns the resolved container engine's name ("podman" or "docker").
func (e *Executor) Engine() string { return e.engineName }

// SetupCluster has nothing to bootstrap: the container engine needs no namespace/priority-class
// equivalent created up front. Present only to keep the same call shape as
// k8sexec.JobWorkloadClient.
func (e *Executor) SetupCluster(_ context.Context) error { return nil }

const machineIDPath = "/etc/machine-id"

// GetClusterID returns the host's /etc/machine-id: set once at OS install, stable for the
// machine's life, the bare-metal equivalent of the k8s runtime's kube-system namespace UID. See
// autoscaler.md's "Cluster identity".
func (e *Executor) GetClusterID(_ context.Context) (string, error) {
	return e.clusterID, nil
}

// readClusterID fails New on a host with no machine id. Read per pass instead, the same host
// would start and then fail every reconcile pass for good.
func readClusterID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("podexec: cluster id is required: read %s: %w", path, err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("podexec: cluster id is required: %s is empty", path)
	}
	return id, nil
}
