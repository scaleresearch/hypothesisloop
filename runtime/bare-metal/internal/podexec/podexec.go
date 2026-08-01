package podexec

import (
	"context"
	"fmt"

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

	ManagedByValue = workloadkeys.ManagedByValue

	// containerNamePrefix keeps this runtime's containers trivially greppable/distinguishable
	// from anything else podman might be running on the node.
	containerNamePrefix = "hypothesisloop-"
)

// Config mirrors k8sexec.Config's shape: every operational value is required explicitly, no
// implicit defaults.
type Config struct {
	RegistryURL string
	// DefaultTerminationGracePeriodSeconds/MaxTerminationGracePeriodSeconds mirror
	// k8sexec.Config exactly — see there for meaning.
	DefaultTerminationGracePeriodSeconds int
	MaxTerminationGracePeriodSeconds     int
	// PricedAcceleratorTypes bounds which accelerator types capacity reporting names (see
	// hypothesisloop.yaml accelerator_types) — identical role to k8sexec.Config.
	PricedAcceleratorTypes []string
	// ScratchDir is the filesystem this node's Storage quota is measured/enforced against
	// (podman --storage-opt size=, statfs'd for total/available). Required.
	ScratchDir string
	// NodeLabels are this node's static facts (zone, disk class, ...) merged with
	// probe-derived accelerator labels when reporting GetNodeLabels — the bare-node analogue
	// of a single fleet.yaml node entry (different-backend.md §4.2).
	NodeLabels map[string]string
	// NodeName overrides the OS hostname as this node's reported identity. Optional.
	NodeName string
}

type Executor struct {
	registryURL                          string
	defaultTerminationGracePeriodSeconds int64
	maxTerminationGracePeriodSeconds     int64
	pricedAcceleratorTypes               map[string]bool
	scratchDir                           string
	nodeLabels                           map[string]string
	// nodeName is this node's own identity, reported as the sole entry in every
	// cluster->node->... map the Backend contract expects (a bare-node "cluster" is one node).
	nodeName string
	// docker is the Engine API client connected to whichever engine resolveEngineClient found
	// (podman's Docker-compatible socket, or dockerd's own) — every container operation this
	// package makes goes through it; nothing here shells out.
	docker *client.Client
	// engineName is the resolved engine's name ("podman" or "docker"), for logging only.
	engineName string
	// storageQuotaEnforced is decided once at startup (see scratchQuotaSupported): whether
	// ScratchDir's filesystem can back a per-container storage-opt size quota. When false,
	// Storage is still a required, recorded, accounted-for field (capacity math still treats it
	// as a hard dimension) but is not passed to the engine as an enforced quota — see
	// BuildContainerSpec.
	storageQuotaEnforced bool
}

func New(cfg Config) (*Executor, error) {
	if cfg.RegistryURL == "" {
		return nil, fmt.Errorf("podexec: RegistryURL is required")
	}
	if cfg.DefaultTerminationGracePeriodSeconds <= 0 || cfg.MaxTerminationGracePeriodSeconds <= 0 {
		return nil, fmt.Errorf("podexec: termination settings must be positive")
	}
	if cfg.ScratchDir == "" {
		return nil, fmt.Errorf("podexec: ScratchDir is required")
	}
	docker, engineName, err := resolveEngineClient(context.Background())
	if err != nil {
		return nil, err
	}
	priced := make(map[string]bool, len(cfg.PricedAcceleratorTypes))
	for _, name := range cfg.PricedAcceleratorTypes {
		priced[name] = true
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
		registryURL:                          cfg.RegistryURL,
		defaultTerminationGracePeriodSeconds: int64(cfg.DefaultTerminationGracePeriodSeconds),
		maxTerminationGracePeriodSeconds:     int64(cfg.MaxTerminationGracePeriodSeconds),
		pricedAcceleratorTypes:               priced,
		scratchDir:                           cfg.ScratchDir,
		nodeLabels:                           labels,
		nodeName:                             nodeName,
		docker:                               docker,
		engineName:                           engineName,
		storageQuotaEnforced:                 quotaSupported,
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

func (e *Executor) ProvisionAgent(_ context.Context, _ string) error { return nil }
