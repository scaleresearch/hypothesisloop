// Package workload provides a Kubernetes workload client for OpenResearch,
// using only native Kubernetes primitives — no external queueing operator.
//
// Admission (deciding whether a job fits available capacity right now) is
// decided entirely by the Go scheduler loop (services/scheduler) *before*
// this client is ever asked to create a Job — so there is no suspend/admit
// handshake to perform here. This client just creates a plain batchv1.Job,
// with a native scheduling.k8s.io/v1 PriorityClass attached so the k8s
// scheduler itself orders/preempts pods correctly under node pressure.
// Two priority classes exist: openresearch-guaranteed, openresearch-burst.
//
// ClusterSet is one implementation of Backend (see backend.go). A team that wants a
// different scheduling mechanism (Kueue, Volcano, Slurm-on-k8s, ...) implements Backend
// in its own package and swaps the constructor call in cmd/*/main.go — no changes needed
// anywhere else, since services/scheduler, services/controller, and services/quota only
// depend on Backend (or narrower local subsets of it), never on ClusterSet directly.
//
// File layout: this file holds Config/New(); clusterset.go holds the multi-cluster
// ClusterSet wrapper; cluster_setup.go holds namespace/PriorityClass bootstrap and
// flavor-capacity config; job_lifecycle.go holds create/delete/poll operations;
// job_defaults.go holds the per-cluster JobDefaults ConfigMap merge; job_build.go holds
// BuildJob and its affinity/toleration helpers; util.go holds small shared helpers.
package workload

import (
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	OpenResearchNamespace = "openresearch-jobs"

	// Native scheduling.k8s.io/v1 PriorityClass names. Higher value preempts lower
	// value at the k8s scheduler level under node resource pressure.
	PriorityClassGuaranteed = "openresearch-guaranteed"
	PriorityClassBurst      = "openresearch-burst"
	priorityValueGuaranteed = int32(1000000)
	priorityValueBurst      = int32(100000)

	RegistryURLDefault = "http://metrics-service:8083"

	// AllocationMode values — mirror config.AllocationModeResource/AllocationModeDRA exactly
	// (plain strings, not the config package's consts, since this package must not import
	// shared/config: OpenResearchConfig already decouples workload from config's Go types so
	// workload stays usable by anything that hands it plain maps, tests included).
	AllocationModeResource = "resource"
	AllocationModeDRA      = "dra"

	// DistributedMasterPort is the fixed rendezvous port advertised to every rank of a
	// distributed (NumNodes > 1) job via OPENRESEARCH_MASTER_PORT — one fixed port is fine
	// since each job gets its own pods/Service, no cross-job collision is possible. Training
	// code chooses how to use it: torchrun's --master-port for PyTorch DDP, Ray's --port for
	// `ray start --head`/`ray start --address=...`, or any other rendezvous scheme.
	DistributedMasterPort = 29500
)

// Defaults applied by New() when the corresponding Config field is zero. These mirror the
// defaults in shared/config so the client behaves sanely when constructed without config
// (tests, local runs).
const (
	// DefaultJobBackoffLimit is how many times k8s retries a failing pod (RestartPolicy=
	// OnFailure) before marking the Job Failed. A persistently crashing job exhausts these
	// retries and is failed natively by k8s (caught by the job watcher → FAILED + refund);
	// a one-off flake is tolerated.
	DefaultJobBackoffLimit = int32(3)
	// DefaultJobDeadlineMultiplier multiplies estimated_duration_hours for ActiveDeadlineSeconds.
	DefaultJobDeadlineMultiplier = 1.5
	// DefaultMinJobDeadlineSeconds is the ActiveDeadlineSeconds floor.
	DefaultMinJobDeadlineSeconds = int64(300)
)

type Config struct {
	KubeconfigPath     string
	KubeContext        string
	RegistryURL        string
	OpenResearchConfig *OpenResearchConfig
	// JobBackoffLimit is the k8s Job BackoffLimit (pod retries before native failure).
	// Zero falls back to JobBackoffLimit default.
	JobBackoffLimit int
	// JobDeadlineMultiplier multiplies estimated_duration_hours for ActiveDeadlineSeconds.
	// Zero falls back to 1.5.
	JobDeadlineMultiplier float64
	// MinJobDeadlineSeconds is the ActiveDeadlineSeconds floor. Zero falls back to 300.
	MinJobDeadlineSeconds int
	// AcceleratorResourceName is the k8s extended resource name requested per accelerator (quantity =
	// JobSpec.AcceleratorCount). Empty falls back to "nvidia.com/gpu". Set to "cpu" as a temporary
	// substitution on a cluster with no accelerator nodes/device plugin yet — agents' JobSpec.AcceleratorCount
	// is unaffected either way, since it's a plain accelerator count, not a k8s resource name.
	AcceleratorResourceName string
	// AcceleratorTaintKey is the taint key real accelerator nodes carry (commonly applied by the NVIDIA GPU
	// Operator so only accelerator-requesting pods land there) — the backend automatically adds a
	// matching toleration to any pod that requests accelerators. Empty falls back to
	// "nvidia.com/gpu". This is purely an execution-engine concern: agents never declare
	// tolerations themselves in JobSpec.
	AcceleratorTaintKey string
}

type OpenResearchConfig struct {
	NameByFlavor map[string]string
	AcceleratorsByFlavor map[string]int
	FlavorOrder  []string
	// NodeLabelByType maps an accelerator type name to the node label value real accelerator nodes of that
	// type carry (set by the vendor's device-plugin/feature-discovery add-on). Used to
	// translate JobSpec.AcceleratorType/AcceptableAcceleratorTypes into a native nodeAffinity — see
	// acceleratorNodeAffinity.
	NodeLabelByType map[string]string
	// NodeLabelKeyByType/ResourceNameByType/TaintKeyByType map an accelerator type name to its own
	// vendor-specific node-label key, k8s extended resource name, and taint key — per-type
	// so a cluster can mix vendors (NVIDIA + AMD) in one catalog. Falls back to
	// "nvidia.com/gpu.product"/acceleratorResourceName/acceleratorTaintKey (the client's own defaults) for
	// any type not present in these maps.
	NodeLabelKeyByType map[string]string
	ResourceNameByType map[string]string
	TaintKeyByType     map[string]string
	// AllocationModeByType/DeviceClassNameByType select, per accelerator type, whether BuildJob
	// requests a classic extended resource (default, "resource") or a Dynamic Resource
	// Allocation ResourceClaimTemplate ("dra", e.g. Tenstorrent's tt-dra-driver) — see
	// config.AcceleratorTypeConfig.AllocationMode's doc for the full rationale. A type absent
	// from AllocationModeByType is treated as "resource" (today's only behavior), so existing
	// NVIDIA/AMD clusters are unaffected.
	AllocationModeByType  map[string]string
	DeviceClassNameByType map[string]string
}

// DefaultAcceleratorResourceName is the k8s extended resource name requested per accelerator when
// Config.AcceleratorResourceName is unset.
const DefaultAcceleratorResourceName = "nvidia.com/gpu"

// DefaultAcceleratorTaintKey is the taint key tolerated on any accelerator-requesting pod when
// Config.AcceleratorTaintKey is unset — matches the taint the NVIDIA GPU Operator applies to accelerator
// nodes by default.
const DefaultAcceleratorTaintKey = "nvidia.com/gpu"

type JobWorkloadClient struct {
	kube                  kubernetes.Interface
	dyn                   dynamic.Interface
	registryURL           string
	pcfg                  *OpenResearchConfig
	jobBackoffLimit       int32
	jobDeadlineMultiplier float64
	minJobDeadlineSeconds int64
	acceleratorResourceName       string
	acceleratorTaintKey           string
	nodeLabelByType       map[string]string
	nodeLabelKeyByType    map[string]string
	resourceNameByType    map[string]string
	taintKeyByType        map[string]string
	allocationModeByType  map[string]string
	deviceClassNameByType map[string]string
}

func New(cfg Config) (*JobWorkloadClient, error) {
	restCfg, err := buildRestConfig(cfg.KubeconfigPath, cfg.KubeContext)
	if err != nil {
		return nil, fmt.Errorf("workload: rest config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("workload: kube client: %w", err)
	}
	// dyn talks to resource.k8s.io as unstructured JSON rather than a generated typed client
	// pinned to one DRA API version — deliberate: k8s.io/client-go v0.31 (this repo's pin) only
	// ships resource.k8s.io/v1alpha3 types, but a modern cluster (e.g. k3s 1.36, GA DRA) serves
	// v1. Going through dynamic.Interface means BuildJob's DRA path works against whatever DRA
	// API version the target cluster actually runs, with zero coupling to this binary's
	// compiled-in k8s.io/api version — the same reason this is the right seam for a future
	// AMD DRA driver or any other resource.k8s.io consumer to reuse untouched.
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("workload: dynamic client: %w", err)
	}
	reg := cfg.RegistryURL
	if reg == "" {
		reg = RegistryURLDefault
	}
	backoff := int32(cfg.JobBackoffLimit)
	if backoff == 0 {
		backoff = DefaultJobBackoffLimit
	}
	deadlineMult := cfg.JobDeadlineMultiplier
	if deadlineMult == 0 {
		deadlineMult = DefaultJobDeadlineMultiplier
	}
	minDeadline := int64(cfg.MinJobDeadlineSeconds)
	if minDeadline == 0 {
		minDeadline = DefaultMinJobDeadlineSeconds
	}
	acceleratorResourceName := cfg.AcceleratorResourceName
	if acceleratorResourceName == "" {
		acceleratorResourceName = DefaultAcceleratorResourceName
	}
	acceleratorTaintKey := cfg.AcceleratorTaintKey
	if acceleratorTaintKey == "" {
		acceleratorTaintKey = DefaultAcceleratorTaintKey
	}
	var nodeLabelByType, nodeLabelKeyByType, resourceNameByType, taintKeyByType map[string]string
	var allocationModeByType, deviceClassNameByType map[string]string
	if cfg.OpenResearchConfig != nil {
		nodeLabelByType = cfg.OpenResearchConfig.NodeLabelByType
		nodeLabelKeyByType = cfg.OpenResearchConfig.NodeLabelKeyByType
		resourceNameByType = cfg.OpenResearchConfig.ResourceNameByType
		taintKeyByType = cfg.OpenResearchConfig.TaintKeyByType
		allocationModeByType = cfg.OpenResearchConfig.AllocationModeByType
		deviceClassNameByType = cfg.OpenResearchConfig.DeviceClassNameByType
	}
	return &JobWorkloadClient{
		kube:                  kube,
		dyn:                   dyn,
		registryURL:           reg,
		pcfg:                  cfg.OpenResearchConfig,
		jobBackoffLimit:       backoff,
		jobDeadlineMultiplier: deadlineMult,
		minJobDeadlineSeconds: minDeadline,
		acceleratorResourceName:       acceleratorResourceName,
		nodeLabelKeyByType:    nodeLabelKeyByType,
		resourceNameByType:    resourceNameByType,
		taintKeyByType:        taintKeyByType,
		acceleratorTaintKey:           acceleratorTaintKey,
		nodeLabelByType:       nodeLabelByType,
		allocationModeByType:  allocationModeByType,
		deviceClassNameByType: deviceClassNameByType,
	}, nil
}
