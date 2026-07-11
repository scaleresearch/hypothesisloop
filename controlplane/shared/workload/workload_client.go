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
package workload

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
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
	// GPUResourceName is the k8s extended resource name requested per GPU (quantity =
	// JobSpec.GPUCount). Empty falls back to "nvidia.com/gpu". Set to "cpu" as a temporary
	// substitution on a cluster with no GPU nodes/device plugin yet — agents' JobSpec.GPUCount
	// is unaffected either way, since it's a plain GPU count, not a k8s resource name.
	GPUResourceName string
	// GPUTaintKey is the taint key real GPU nodes carry (commonly applied by the NVIDIA GPU
	// Operator so only GPU-requesting pods land there) — the backend automatically adds a
	// matching toleration to any pod that requests GPUs. Empty falls back to
	// "nvidia.com/gpu". This is purely an execution-engine concern: agents never declare
	// tolerations themselves in JobSpec.
	GPUTaintKey string
}

type OpenResearchConfig struct {
	NameByFlavor map[string]string
	GPUsByFlavor map[string]int
	FlavorOrder  []string
	// NodeLabelByType maps a GPU type name to the node label value real GPU nodes of that
	// type carry (set by the vendor's device-plugin/feature-discovery add-on). Used to
	// translate JobSpec.GPUType/AcceptableGPUTypes into a native nodeAffinity — see
	// gpuNodeAffinity.
	NodeLabelByType map[string]string
	// NodeLabelKeyByType/ResourceNameByType/TaintKeyByType map a GPU type name to its own
	// vendor-specific node-label key, k8s extended resource name, and taint key — per-type
	// so a cluster can mix vendors (NVIDIA + AMD) in one catalog. Falls back to
	// "nvidia.com/gpu.product"/gpuResourceName/gpuTaintKey (the client's own defaults) for
	// any type not present in these maps.
	NodeLabelKeyByType map[string]string
	ResourceNameByType map[string]string
	TaintKeyByType     map[string]string
}

// DefaultGPUResourceName is the k8s extended resource name requested per GPU when
// Config.GPUResourceName is unset.
const DefaultGPUResourceName = "nvidia.com/gpu"

// DefaultGPUTaintKey is the taint key tolerated on any GPU-requesting pod when
// Config.GPUTaintKey is unset — matches the taint the NVIDIA GPU Operator applies to GPU
// nodes by default.
const DefaultGPUTaintKey = "nvidia.com/gpu"

type JobWorkloadClient struct {
	kube                  kubernetes.Interface
	registryURL           string
	pcfg                  *OpenResearchConfig
	jobBackoffLimit       int32
	jobDeadlineMultiplier float64
	minJobDeadlineSeconds int64
	gpuResourceName       string
	gpuTaintKey           string
	nodeLabelByType       map[string]string
	nodeLabelKeyByType    map[string]string
	resourceNameByType    map[string]string
	taintKeyByType        map[string]string
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
	gpuResourceName := cfg.GPUResourceName
	if gpuResourceName == "" {
		gpuResourceName = DefaultGPUResourceName
	}
	gpuTaintKey := cfg.GPUTaintKey
	if gpuTaintKey == "" {
		gpuTaintKey = DefaultGPUTaintKey
	}
	var nodeLabelByType, nodeLabelKeyByType, resourceNameByType, taintKeyByType map[string]string
	if cfg.OpenResearchConfig != nil {
		nodeLabelByType = cfg.OpenResearchConfig.NodeLabelByType
		nodeLabelKeyByType = cfg.OpenResearchConfig.NodeLabelKeyByType
		resourceNameByType = cfg.OpenResearchConfig.ResourceNameByType
		taintKeyByType = cfg.OpenResearchConfig.TaintKeyByType
	}
	return &JobWorkloadClient{
		kube:                  kube,
		registryURL:           reg,
		pcfg:                  cfg.OpenResearchConfig,
		jobBackoffLimit:       backoff,
		jobDeadlineMultiplier: deadlineMult,
		minJobDeadlineSeconds: minDeadline,
		gpuResourceName:       gpuResourceName,
		nodeLabelKeyByType:    nodeLabelKeyByType,
		resourceNameByType:    resourceNameByType,
		taintKeyByType:        taintKeyByType,
		gpuTaintKey:           gpuTaintKey,
		nodeLabelByType:       nodeLabelByType,
	}, nil
}

// DefaultClusterName is the cluster name used when only a single cluster is configured
// (the historical single-cluster behavior) or when an experiment has no ClusterName set
// (old rows, single-cluster mode).
const DefaultClusterName = "default"

// ClusterSet holds one *JobWorkloadClient per configured target Kubernetes cluster, keyed
// by cluster name. It is the multi-cluster-aware entry point that call sites should use
// instead of holding a single *JobWorkloadClient directly.
type ClusterSet struct {
	clients map[string]*JobWorkloadClient
	order   []string
}

// NewClusterSet builds a ClusterSet from a map of per-cluster Config, calling the existing
// New() for each entry. The map keys are cluster names (e.g. "default").
func NewClusterSet(configs map[string]Config) (*ClusterSet, error) {
	if len(configs) == 0 {
		return nil, fmt.Errorf("workload: NewClusterSet: no cluster configs provided")
	}
	cs := &ClusterSet{clients: make(map[string]*JobWorkloadClient, len(configs))}
	// Sort names for deterministic iteration order (Names(), first-cluster policies, etc).
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		client, err := New(configs[name])
		if err != nil {
			return nil, fmt.Errorf("workload: NewClusterSet: cluster %q: %w", name, err)
		}
		cs.clients[name] = client
		cs.order = append(cs.order, name)
	}
	return cs, nil
}

// Get returns the client for the named cluster, if configured.
func (cs *ClusterSet) Get(name string) (*JobWorkloadClient, bool) {
	c, ok := cs.clients[name]
	return c, ok
}

// Default returns the client to use when a call site has no explicit cluster routing
// (backward compatibility for single-cluster deployments and old rows without a
// ClusterName). If there is exactly one configured cluster, that one is returned
// regardless of its name. Otherwise the cluster named "default" is returned, if present.
func (cs *ClusterSet) Default() *JobWorkloadClient {
	if len(cs.clients) == 1 {
		for _, c := range cs.clients {
			return c
		}
	}
	return cs.clients[DefaultClusterName]
}

// Names returns the configured cluster names in stable (sorted) order.
func (cs *ClusterSet) Names() []string {
	out := make([]string, len(cs.order))
	copy(out, cs.order)
	return out
}

// forExperiment resolves the client to use for exp, routing by exp.ClusterName and
// falling back to Default() when it is empty (old rows / single-cluster mode) or unknown.
func (cs *ClusterSet) forExperiment(exp *domain.Experiment) *JobWorkloadClient {
	if exp != nil && exp.ClusterName != "" {
		if c, ok := cs.clients[exp.ClusterName]; ok {
			return c
		}
	}
	return cs.Default()
}

// CreateWorkload routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) CreateWorkload(ctx context.Context, exp *domain.Experiment) error {
	c := cs.forExperiment(exp)
	if c == nil {
		return fmt.Errorf("workload: ClusterSet.CreateWorkload: no client for cluster %q", exp.ClusterName)
	}
	return c.CreateWorkload(ctx, exp)
}

// DeleteWorkload routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) DeleteWorkload(ctx context.Context, exp *domain.Experiment) error {
	c := cs.forExperiment(exp)
	if c == nil {
		return fmt.Errorf("workload: ClusterSet.DeleteWorkload: no client for cluster %q", exp.ClusterName)
	}
	return c.DeleteWorkload(ctx, exp.ID)
}

// PollJobPhase routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) PollJobPhase(ctx context.Context, exp *domain.Experiment) (JobPhase, error) {
	c := cs.forExperiment(exp)
	if c == nil {
		return JobPhasePending, fmt.Errorf("workload: ClusterSet.PollJobPhase: no client for cluster %q", exp.ClusterName)
	}
	return c.PollJobPhase(ctx, exp.ID)
}

// GetAdmittedGPUType routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) GetAdmittedGPUType(ctx context.Context, exp *domain.Experiment) domain.GPUType {
	c := cs.forExperiment(exp)
	if c == nil {
		return exp.GPUType
	}
	return c.GetAdmittedGPUType(ctx, exp)
}

// WaitForJobDeletion routes to the cluster named by exp.ClusterName (or the default cluster).
func (cs *ClusterSet) WaitForJobDeletion(ctx context.Context, exp *domain.Experiment, timeout time.Duration) error {
	c := cs.forExperiment(exp)
	if c == nil {
		return fmt.Errorf("workload: ClusterSet.WaitForJobDeletion: no client for cluster %q", exp.ClusterName)
	}
	return c.WaitForJobDeletion(ctx, exp.ID, timeout)
}

// GetFlavorCapacity returns nominal GPU slot capacity per flavor. Capacity is derived
// from static config (not live cluster state) and is identical across clusters today, so
// this reports the default cluster's view — matching pre-multi-cluster behavior exactly.
func (cs *ClusterSet) GetFlavorCapacity(ctx context.Context) (guaranteed, burst map[string]int64, err error) {
	c := cs.Default()
	if c == nil {
		return nil, nil, fmt.Errorf("workload: ClusterSet.GetFlavorCapacity: no default cluster configured")
	}
	return c.GetFlavorCapacity(ctx)
}

// ClusterNames is an alias for Names(), satisfying call sites that pick a target cluster
// for admission (e.g. services/scheduler.WorkloadClient).
func (cs *ClusterSet) ClusterNames() []string {
	return cs.Names()
}

// ProvisionAgent runs on the default cluster: today's native backend has no per-agent
// object to create (see Backend.ProvisionAgent doc), so this is a no-op passthrough.
func (cs *ClusterSet) ProvisionAgent(ctx context.Context, agentID string) error {
	c := cs.Default()
	if c == nil {
		return fmt.Errorf("workload: ClusterSet.ProvisionAgent: no default cluster configured")
	}
	return c.ProvisionAgent(ctx, agentID)
}

// SetupCluster runs SetupCluster on every configured cluster.
func (cs *ClusterSet) SetupCluster(ctx context.Context) error {
	for _, name := range cs.order {
		if err := cs.clients[name].SetupCluster(ctx); err != nil {
			return fmt.Errorf("workload: ClusterSet.SetupCluster: cluster %q: %w", name, err)
		}
	}
	return nil
}

var defaultFlavorOrder = []string{"flavor-t4", "flavor-l40", "flavor-a100", "flavor-h100", "flavor-h200"}

var defaultNameByFlavor = map[string]string{
	"flavor-t4": "T4", "flavor-l40": "L40", "flavor-a100": "A100",
	"flavor-h100": "H100", "flavor-h200": "H200",
}

var defaultGPUsByFlavor = map[string]int{
	"flavor-t4": 64, "flavor-l40": 16, "flavor-a100": 8, "flavor-h100": 4, "flavor-h200": 2,
}

func (c *JobWorkloadClient) flavorOrder() []string {
	if c.pcfg != nil {
		return c.pcfg.FlavorOrder
	}
	return defaultFlavorOrder
}

func (c *JobWorkloadClient) nameByFlavor() map[string]string {
	if c.pcfg != nil {
		return c.pcfg.NameByFlavor
	}
	return defaultNameByFlavor
}

func (c *JobWorkloadClient) gpusByFlavor() map[string]int {
	if c.pcfg != nil {
		return c.pcfg.GPUsByFlavor
	}
	return defaultGPUsByFlavor
}

// gpuNominalCapacity returns the nominal GPU slot count per flavor from config (or the
// PoC defaults). This is the only resource dimension the scheduler admits/preempts on —
// CPU, memory, and storage are not modeled.
func (c *JobWorkloadClient) gpuNominalCapacity() map[string]int64 {
	gpus := c.gpusByFlavor()
	out := make(map[string]int64, len(gpus))
	for flavor, count := range gpus {
		out[flavor] = int64(count)
	}
	return out
}

// SetupCluster ensures the namespace and the two native PriorityClasses exist. There is no
// operator to install and no queue/cohort/flavor topology to reconcile — capacity accounting
// happens in Go (services/scheduler), not as Kubernetes objects.
func (c *JobWorkloadClient) SetupCluster(ctx context.Context) error {
	if err := c.ensureNamespace(ctx, OpenResearchNamespace); err != nil {
		return err
	}
	return c.ensurePriorityClasses(ctx)
}

func (c *JobWorkloadClient) ensureNamespace(ctx context.Context, name string) error {
	_, err := c.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("workload: create namespace %s: %w", name, err)
	}
	return nil
}

// ensurePriorityClasses creates the two native scheduling.k8s.io/v1 PriorityClass objects
// used to order/preempt pods at the k8s scheduler level. These are cluster-scoped and
// idempotent to (re-)create.
func (c *JobWorkloadClient) ensurePriorityClasses(ctx context.Context) error {
	classes := []struct {
		name  string
		value int32
	}{
		{PriorityClassGuaranteed, priorityValueGuaranteed},
		{PriorityClassBurst, priorityValueBurst},
	}
	for _, pc := range classes {
		obj := &schedulingv1.PriorityClass{
			ObjectMeta:    metav1.ObjectMeta{Name: pc.name},
			Value:         pc.value,
			GlobalDefault: false,
			Description:   "OpenResearch capacity tier priority class (managed in-code, not by an operator).",
		}
		_, err := c.kube.SchedulingV1().PriorityClasses().Create(ctx, obj, metav1.CreateOptions{})
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("workload: create PriorityClass %s: %w", pc.name, err)
		}
	}
	return nil
}

func (c *JobWorkloadClient) CreateWorkload(ctx context.Context, exp *domain.Experiment) error {
	if err := c.ensureNamespace(ctx, OpenResearchNamespace); err != nil {
		return err
	}
	// No admission handshake here: the scheduler loop (services/scheduler) already decided
	// this experiment fits available capacity before calling CreateWorkload, so the Job is
	// created ready-to-run (not suspended).
	job, err := c.BuildJob(ctx, exp)
	if err != nil {
		return err
	}
	if job.Spec.Template.Spec.Subdomain != "" {
		if err := c.ensureHeadlessService(ctx, job); err != nil {
			return fmt.Errorf("workload: ensure headless service: %w", err)
		}
	}
	_, err = c.kube.BatchV1().Jobs(OpenResearchNamespace).Create(ctx, job, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// ensureHeadlessService creates the headless (ClusterIP: None) Service a distributed job's
// pods need for rank 0's stable DNS name (see OPENRESEARCH_MASTER_ADDR in BuildJob) — named
// identically to the Job and selecting its pods by experiment-id label, matching the
// Subdomain BuildJob sets on the pod template.
func (c *JobWorkloadClient) ensureHeadlessService(ctx context.Context, job *batchv1.Job) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: OpenResearchNamespace,
			Labels:    job.Labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  job.Spec.Template.Labels,
		},
	}
	_, err := c.kube.CoreV1().Services(OpenResearchNamespace).Create(ctx, svc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// ListManagedJobs returns the experiment IDs of every Job this client manages
// (labeled openresearch.io/managed-by=openresearch) in the current cluster — used by
// cluster-agent to discover what actually exists, to diff against desired state.
func (c *JobWorkloadClient) ListManagedJobs(ctx context.Context) ([]string, error) {
	jobs, err := c.kube.BatchV1().Jobs(OpenResearchNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "openresearch.io/managed-by=openresearch",
	})
	if err != nil {
		return nil, fmt.Errorf("workload: list managed jobs: %w", err)
	}
	ids := make([]string, 0, len(jobs.Items))
	for _, j := range jobs.Items {
		if id := j.Labels["openresearch.io/experiment-id"]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// GetLiveCPUCapacity returns this cluster's real, current CPU-core capacity: total allocatable
// cores across schedulable nodes, and the same minus every non-terminal pod's CPU requests
// cluster-wide (not just openresearch-managed pods — a true "how much is actually free" number,
// the same thing a real scheduler would compute). Replaces the old static-config-only capacity
// model for CPU-only jobs; pushed to the control plane on every desired-state poll.
func (c *JobWorkloadClient) GetLiveCPUCapacity(ctx context.Context) (available, total float64, err error) {
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("workload: list nodes: %w", err)
	}
	var totalMilli int64
	for _, n := range nodes.Items {
		if n.Spec.Unschedulable {
			continue
		}
		if q, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
			totalMilli += q.MilliValue()
		}
	}

	pods, err := c.kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("workload: list pods: %w", err)
	}
	var requestedMilli int64
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, ctr := range p.Spec.Containers {
			if q, ok := ctr.Resources.Requests[corev1.ResourceCPU]; ok {
				requestedMilli += q.MilliValue()
			}
		}
	}

	availMilli := totalMilli - requestedMilli
	if availMilli < 0 {
		availMilli = 0
	}
	return float64(availMilli) / 1000.0, float64(totalMilli) / 1000.0, nil
}

func (c *JobWorkloadClient) DeleteWorkload(ctx context.Context, experimentID string) error {
	prop := metav1.DeletePropagationBackground
	err := c.kube.BatchV1().Jobs(OpenResearchNamespace).Delete(ctx, jobName(experimentID), metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	// The headless Service (see ensureHeadlessService) is only ever created for distributed
	// jobs, but deleting it unconditionally here is harmless (NotFound is swallowed) and
	// avoids needing to know NumNodes at this call site.
	svcErr := c.kube.CoreV1().Services(OpenResearchNamespace).Delete(ctx, jobName(experimentID), metav1.DeleteOptions{})
	if svcErr != nil && !errors.IsNotFound(svcErr) {
		return svcErr
	}
	return nil
}

func (c *JobWorkloadClient) WaitForJobDeletion(ctx context.Context, experimentID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	name := jobName(experimentID)
	for time.Now().Before(deadline) {
		_, err := c.kube.BatchV1().Jobs(OpenResearchNamespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("workload: timed out waiting for job %s deletion", name)
}

// GetFlavorCapacity returns nominal GPU slot capacity per flavor from config.
// demo: not reading live reservation state from the GPU cluster
func (c *JobWorkloadClient) GetFlavorCapacity(_ context.Context) (guaranteed, burst map[string]int64, err error) {
	nominal := c.gpuNominalCapacity()
	guaranteed = make(map[string]int64, len(nominal))
	burst = make(map[string]int64, len(nominal))
	for flavor, n := range nominal {
		guaranteed[flavor] = n
		burst[flavor] = n
	}
	return guaranteed, burst, nil
}

type JobPhase int

const (
	JobPhasePending JobPhase = iota
	JobPhaseRunning
	JobPhaseSucceeded
	JobPhaseFailed
	JobPhaseGone
)

// String renders the phase as a stable lowercase name — used when persisting/transmitting
// phase over the wire (e.g. cluster-agent status reports), where the int representation
// would not be a meaningful value across processes.
func (p JobPhase) String() string {
	switch p {
	case JobPhaseRunning:
		return "running"
	case JobPhaseSucceeded:
		return "succeeded"
	case JobPhaseFailed:
		return "failed"
	case JobPhaseGone:
		return "gone"
	default:
		return "pending"
	}
}

// ParseJobPhase is the inverse of JobPhase.String, used to decode phase values received
// over the wire. Unrecognized values decode to JobPhasePending (fail safe, not fail loud —
// an unrecognized phase should never be treated as terminal).
func ParseJobPhase(s string) JobPhase {
	switch s {
	case "running":
		return JobPhaseRunning
	case "succeeded":
		return JobPhaseSucceeded
	case "failed":
		return JobPhaseFailed
	case "gone":
		return JobPhaseGone
	default:
		return JobPhasePending
	}
}

func (c *JobWorkloadClient) PollJobPhase(ctx context.Context, experimentID string) (JobPhase, error) {
	job, err := c.kube.BatchV1().Jobs(OpenResearchNamespace).Get(ctx, jobName(experimentID), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return JobPhaseGone, nil
		}
		return JobPhasePending, err
	}
	// Check the native JobComplete condition before falling back to a raw Succeeded
	// count: for Indexed jobs with a SuccessPolicy (distributed training gated on rank 0),
	// a non-master worker finishing first bumps Status.Succeeded without the job actually
	// being done — JobComplete only flips true once the configured policy is satisfied.
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return JobPhaseSucceeded, nil
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return JobPhaseFailed, nil
		}
	}
	if job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != batchv1.IndexedCompletion {
		if job.Status.Succeeded > 0 {
			return JobPhaseSucceeded, nil
		}
	}
	if job.Status.Active > 0 {
		return JobPhaseRunning, nil
	}
	return JobPhasePending, nil
}

// GetJobUID returns the k8s Job's UID for experimentID, or "" if the Job doesn't exist. Used by
// cluster-agent to attach ownership-verification data to status pushes: the control plane
// trusts the first UID it sees per experiment and flags (doesn't silently accept) a later
// report carrying a different UID — e.g. a name collision, a stray manually-created Job, or a
// second cluster-agent misconfigured against the same cluster (see
// competetors/SYNTHESIS_GAPS_AND_PLAN.md item #5).
func (c *JobWorkloadClient) GetJobUID(ctx context.Context, experimentID string) (string, error) {
	job, err := c.kube.BatchV1().Jobs(OpenResearchNamespace).Get(ctx, jobName(experimentID), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return string(job.UID), nil
}

// GetAdmittedGPUType always returns the requested GPU type: with no external queueing
// operator in the loop, nothing ever substitutes a different flavor after the control
// plane's own admission decision — the Job is created for exactly the flavor requested.
// Kept as a method (rather than removed) so callers that historically handled flavor
// substitution (services/scheduler/job_watcher.go) keep working unchanged; it is now a
// no-op passthrough.
func (c *JobWorkloadClient) GetAdmittedGPUType(_ context.Context, exp *domain.Experiment) domain.GPUType {
	return exp.GPUType
}

func (c *JobWorkloadClient) ProvisionAgent(_ context.Context, _ string) error { return nil }

// JobDefaults are the operator-owned, per-cluster fallback values merged into an agent's
// JobSpec for anything it left unset. They live in-cluster as a ConfigMap (see
// jobDefaultsConfigMapName below and cluster/infra/job-defaults-configmap.yaml) rather than
// in the cross-cluster control-plane config, because they are inherently a property of one
// specific cluster's execution engine (e.g. "this cluster has no GPU nodes yet, so default
// every job's GPU request to the 'cpu' resource instead") — exactly the kind of concern
// that belongs downlevel, behind the Backend seam, not in the agent-facing DSL.
type JobDefaults struct {
	CPU        string
	Memory     string
	Storage    string
	MaxRetries int
}

// defaultJobDefaults is the fallback used when no ConfigMap is present (fresh clusters,
// local/dev runs) so job submission still works out of the box.
func defaultJobDefaults() JobDefaults {
	return JobDefaults{CPU: "2", Memory: "8Gi", Storage: "5Gi", MaxRetries: 3}
}

// jobDefaultsConfigMapName is the well-known in-cluster ConfigMap operators edit to tune
// this cluster's fallback resource shape, read fresh on every job build (no controller,
// no restart required to pick up a change — just `kubectl edit configmap` in-cluster).
const jobDefaultsConfigMapName = "openresearch-job-defaults"

// jobDefaults fetches this cluster's JobDefaults ConfigMap, falling back to
// defaultJobDefaults() if it doesn't exist or is malformed — a missing ConfigMap is not an
// error, just "use the built-in fallback."
func (c *JobWorkloadClient) jobDefaults(ctx context.Context) JobDefaults {
	def := defaultJobDefaults()
	cm, err := c.kube.CoreV1().ConfigMaps(OpenResearchNamespace).Get(ctx, jobDefaultsConfigMapName, metav1.GetOptions{})
	if err != nil {
		return def
	}
	if v, ok := cm.Data["cpu"]; ok && v != "" {
		def.CPU = v
	}
	if v, ok := cm.Data["memory"]; ok && v != "" {
		def.Memory = v
	}
	if v, ok := cm.Data["storage"]; ok && v != "" {
		def.Storage = v
	}
	if v, ok := cm.Data["max_retries"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			def.MaxRetries = n
		}
	}
	return def
}

// mergeJobDefaults fills every unset field of an agent's JobSpec from this cluster's
// JobDefaults — the merge step that lets agents omit anything they don't care about
// instead of restating the operator's own cluster-shape decisions.
func mergeJobDefaults(job domain.JobSpec, def JobDefaults) domain.JobSpec {
	if job.CPU == "" {
		job.CPU = def.CPU
	}
	if job.Memory == "" {
		job.Memory = def.Memory
	}
	if job.Storage == "" {
		job.Storage = def.Storage
	}
	if job.MaxRetries == nil {
		job.MaxRetries = &def.MaxRetries
	}
	return job
}

// GPUTypeLabel tracks which GPU type/tier this run is billed against — set by the control
// plane from exp.GPUType, purely for observability (kubectl get jobs -l ...); it is never
// read back to derive accounting, since exp.GPUType/exp.GPUCount are already known from the
// agent's JobSpec at submission time.
const GPUTypeLabel = "openresearch.io/gpu-type"

// BuildJob compiles exp's JobSpec DSL (merged with this cluster's JobDefaults) down into a
// native batch/v1 Job — the only place the platform's DSL vocabulary (image, command, env,
// cpu/memory/gpu, num_nodes, max_retries, acceptable_gpu_types) is translated into
// execution-engine concepts (containers, resource requests, node affinity, Indexed
// completion mode). Nothing upstream of this function ever constructs or reads a k8s type.
//
// Distributed jobs (JobSpec.NumNodes > 1) get native Indexed completion mode, per-index
// retry, an OOM-aware pod failure policy, and rank/world-size env vars, matching torchrun/
// Horovod's RANK/WORLD_SIZE convention.
func (c *JobWorkloadClient) BuildJob(ctx context.Context, exp *domain.Experiment) (*batchv1.Job, error) {
	spec := mergeJobDefaults(exp.Job, c.jobDefaults(ctx))

	nodes := spec.NumNodes
	if nodes < 1 {
		nodes = 1
	}
	parallelism := int32(nodes)
	distributed := parallelism > 1

	backoff := c.jobBackoffLimit
	if spec.MaxRetries != nil {
		backoff = int32(*spec.MaxRetries)
	}

	deadline := int64(exp.EstimatedDurationHours * c.jobDeadlineMultiplier * 3600)
	if deadline < c.minJobDeadlineSeconds {
		deadline = c.minJobDeadlineSeconds
	}

	priorityClass := PriorityClassGuaranteed
	if exp.CapacityTier == domain.CapacityBurst {
		priorityClass = PriorityClassBurst
	}

	restartPolicy := corev1.RestartPolicyOnFailure
	if distributed {
		// PodFailurePolicy (below) requires RestartPolicy=Never (k8s API validation):
		// failed containers must surface as failed pods so the Job controller's
		// per-index failure accounting sees them, rather than being retried in-place by
		// the kubelet.
		restartPolicy = corev1.RestartPolicyNever
	}

	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	if spec.CPU != "" {
		resources.Requests[corev1.ResourceCPU] = resource.MustParse(spec.CPU)
		resources.Limits[corev1.ResourceCPU] = resource.MustParse(spec.CPU)
	}
	if spec.Memory != "" {
		resources.Requests[corev1.ResourceMemory] = resource.MustParse(spec.Memory)
		resources.Limits[corev1.ResourceMemory] = resource.MustParse(spec.Memory)
	}
	if spec.Storage != "" {
		resources.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse(spec.Storage)
		resources.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse(spec.Storage)
	}
	if spec.GPUCount > 0 {
		gpuQty := resource.MustParse(fmt.Sprintf("%d", spec.GPUCount))
		gpuResource := corev1.ResourceName(c.resourceNameFor(spec.GPUType))
		resources.Requests[gpuResource] = gpuQty
		resources.Limits[gpuResource] = gpuQty
	}
	for name, qty := range spec.ExtraResources {
		if qty == "" {
			continue
		}
		q, err := resource.ParseQuantity(qty)
		if err != nil {
			return nil, fmt.Errorf("workload: extra_resources[%q]: %w", name, err)
		}
		resources.Requests[corev1.ResourceName(name)] = q
		resources.Limits[corev1.ResourceName(name)] = q
	}

	env := []corev1.EnvVar{
		{Name: "OPENRESEARCH_EXPERIMENT_ID", Value: exp.ID},
		{Name: "OPENRESEARCH_AGENT_ID", Value: exp.AgentID},
		{Name: "OPENRESEARCH_PROJECT_ID", Value: exp.ProjectID},
		{Name: "OPENRESEARCH_CODE_REF", Value: exp.CodeRef},
		{Name: "OPENRESEARCH_CONFIG_HASH", Value: exp.ConfigHash},
		{Name: "OPENRESEARCH_DATA_REF", Value: exp.DataRef},
		{Name: "OPENRESEARCH_REGISTRY_URL", Value: c.registryURL},
		{Name: "OPENRESEARCH_GPU_TYPE", Value: string(exp.GPUType)},
		{Name: "OPENRESEARCH_GPU_COUNT", Value: fmt.Sprintf("%d", exp.GPUCount)},
		{Name: "OPENRESEARCH_DURATION_SECONDS", Value: fmt.Sprintf("%d", int(exp.EstimatedDurationHours*3600))},
		{Name: "TRACEPARENT", Value: traceparentFromID(exp.ID)},
	}
	for k, v := range spec.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	if distributed {
		// OPENRESEARCH_MASTER_ADDR is rank 0's stable DNS name — resolvable because BuildJob
		// sets pod.Spec.Subdomain to the job's own name and CreateWorkload creates a matching
		// headless Service, so k8s's Indexed Job hostname convention
		// ($job-name-$index.$subdomain.$namespace.svc.cluster.local) applies. This is the
		// rendezvous endpoint distributed training/orchestration frameworks need: PyTorch DDP
		// (torchrun --master-addr/--master-port), Ray (`ray start --head` on rank 0,
		// `ray start --address=$OPENRESEARCH_MASTER_ADDR:$OPENRESEARCH_MASTER_PORT` on the
		// rest), or any other rendezvous scheme the training code implements.
		masterAddr := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", jobName(exp.ID), jobName(exp.ID), OpenResearchNamespace)
		env = append(env,
			corev1.EnvVar{
				Name: "OPENRESEARCH_RANK",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.annotations['batch.kubernetes.io/job-completion-index']",
					},
				},
			},
			corev1.EnvVar{Name: "OPENRESEARCH_WORLD_SIZE", Value: fmt.Sprintf("%d", parallelism)},
			corev1.EnvVar{Name: "OPENRESEARCH_MASTER_ADDR", Value: masterAddr},
			corev1.EnvVar{Name: "OPENRESEARCH_MASTER_PORT", Value: fmt.Sprintf("%d", DistributedMasterPort)},
		)
	}

	container := corev1.Container{
		Name:  "experiment",
		Image: spec.Image,
		// IfNotPresent rather than k8s's own default (Always for :latest-tagged images):
		// without this, a locally-imported/locally-built image is ignored and the kubelet
		// always attempts a registry pull, which fails hard on any cluster (dev or air-gapped
		// production) that doesn't have a real registry behind the image reference.
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         spec.Command,
		Args:            spec.Args,
		Env:             env,
		Resources:       resources,
	}

	var volumes []corev1.Volume
	if spec.ShmSize != "" {
		// PyTorch's multiprocess DataLoader and NCCL's shared-memory IPC both need real
		// /dev/shm — k8s defaults it to a tiny tmpfs that silently breaks both under load.
		shmQty := resource.MustParse(spec.ShmSize)
		volumes = append(volumes, corev1.Volume{
			Name: "dshm",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: &shmQty,
				},
			},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "dshm",
			MountPath: "/dev/shm",
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(exp.ID),
			Namespace: OpenResearchNamespace,
			Labels: map[string]string{
				"openresearch.io/managed-by":    "openresearch",
				"openresearch.io/experiment-id": exp.ID,
				"openresearch.io/agent-id":      sanitizeLabel(exp.AgentID),
				"openresearch.io/capacity-tier": string(exp.CapacityTier),
				GPUTypeLabel:                    string(exp.GPUType),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoff,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"openresearch.io/experiment-id": exp.ID},
				},
				Spec: corev1.PodSpec{
					PriorityClassName: priorityClass,
					RestartPolicy:     restartPolicy,
					Containers:        []corev1.Container{container},
					Volumes:           volumes,
					Affinity:          c.buildAffinity(exp.ID, spec, distributed),
					Tolerations:       gpuTolerations(spec.GPUCount, c.taintKeyFor(spec.GPUType)),
				},
			},
		},
	}

	if distributed {
		// Subdomain must match the headless Service CreateWorkload creates alongside this
		// Job (same name) — that's what makes the OPENRESEARCH_MASTER_ADDR DNS name above
		// actually resolve; see the Indexed Job hostname convention this relies on.
		job.Spec.Template.Spec.Subdomain = job.Name

		// Each rank gets its own retry budget: a flaky worker doesn't burn through the
		// whole job's shared backoff budget, and a worker that exhausts its own retries
		// fails just that index rather than nuking every other rank's progress.
		completionMode := batchv1.IndexedCompletion
		job.Spec.CompletionMode = &completionMode
		job.Spec.Completions = &parallelism
		job.Spec.Parallelism = &parallelism
		job.Spec.BackoffLimitPerIndex = &backoff
		// The job is done once rank 0 (the master/coordinator) succeeds — stragglers on
		// other ranks shouldn't hold up completion reporting in job_watcher.go.
		job.Spec.SuccessPolicy = &batchv1.SuccessPolicy{
			Rules: []batchv1.SuccessPolicyRule{{SucceededIndexes: strPtr("0")}},
		}
		// Exit code 137 (SIGKILL, typically OOM) is a hard per-index failure rather than
		// counted toward BackoffLimitPerIndex retries — retrying an OOM with the same
		// resources just wastes GPU-hours on a guaranteed repeat failure.
		job.Spec.PodFailurePolicy = &batchv1.PodFailurePolicy{
			Rules: []batchv1.PodFailurePolicyRule{{
				Action: batchv1.PodFailurePolicyActionFailIndex,
				OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
					Operator: batchv1.PodFailurePolicyOnExitCodesOpIn,
					Values:   []int32{137},
				},
			}},
		}
	}

	return job, nil
}

func strPtr(s string) *string { return &s }

// buildAffinity translates the DSL's hardware/placement vocabulary — GPUSpec.AcceptableGPUTypes
// and JobSpec.Topology — into the native affinity rules that make real multi-host GPU
// training actually work: which GPU models a node must advertise, and how this job's own
// ranks must (or should) be placed relative to each other. Returns nil when nothing was
// requested, so a plain single-node/single-GPU-type job gets no affinity section at all.
// resourceNameFor/taintKeyFor/nodeLabelKeyFor return gpuType's own vendor plumbing (from the
// per-type maps built from openresearch.yaml's gpu_types entries — see config.GPUTypeConfig),
// falling back to the client's cluster-wide default when gpuType has no entry (old rows,
// GPU-less dev clusters using the "cpu" substitution).
func (c *JobWorkloadClient) resourceNameFor(gpuType domain.GPUType) string {
	if v, ok := c.resourceNameByType[string(gpuType)]; ok && v != "" {
		return v
	}
	return c.gpuResourceName
}

func (c *JobWorkloadClient) taintKeyFor(gpuType domain.GPUType) string {
	if v, ok := c.taintKeyByType[string(gpuType)]; ok && v != "" {
		return v
	}
	return c.gpuTaintKey
}

func (c *JobWorkloadClient) nodeLabelKeyFor(gpuType domain.GPUType) string {
	if v, ok := c.nodeLabelKeyByType[string(gpuType)]; ok && v != "" {
		return v
	}
	return "nvidia.com/gpu.product"
}

func (c *JobWorkloadClient) buildAffinity(experimentID string, spec domain.JobSpec, distributed bool) *corev1.Affinity {
	aff := &corev1.Affinity{
		NodeAffinity:    c.gpuNodeAffinity(spec.GPUType, spec.AcceptableGPUTypes),
		PodAffinity:     nil,
		PodAntiAffinity: nil,
	}

	if distributed {
		selector := &metav1.LabelSelector{
			MatchLabels: map[string]string{"openresearch.io/experiment-id": experimentID},
		}

		spreadAcrossHosts := true
		sameZone := false
		if spec.Topology != nil {
			if spec.Topology.SpreadAcrossHosts != nil {
				spreadAcrossHosts = *spec.Topology.SpreadAcrossHosts
			}
			sameZone = spec.Topology.SameZone
		}

		if spreadAcrossHosts {
			// Hard requirement: no two ranks of this job may share a physical host. This
			// is trivially satisfiable for the very first rank scheduled (no sibling pods
			// exist yet to conflict with), so it never risks the job going unschedulable —
			// unlike a same-zone *requirement* would (see below).
			aff.PodAntiAffinity = &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					LabelSelector: selector,
					TopologyKey:   "kubernetes.io/hostname",
				}},
			}
		}
		if sameZone {
			// Soft preference, not a requirement: the first rank scheduled has no sibling
			// pod yet to co-locate with, so a *required* same-zone affinity would make
			// that first pod permanently unschedulable. Preferred affinity instead nudges
			// later ranks toward whichever zone the first ranks land in, minimizing
			// cross-zone network latency for gradient all-reduce without any deadlock risk.
			aff.PodAffinity = &corev1.PodAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: selector,
						TopologyKey:   "topology.kubernetes.io/zone",
					},
				}},
			}
		}
	}

	if aff.NodeAffinity == nil && aff.PodAffinity == nil && aff.PodAntiAffinity == nil {
		return nil
	}
	return aff
}

// gpuTolerations tolerates the taint real GPU nodes carry (commonly applied by the NVIDIA
// GPU Operator, e.g. nvidia.com/gpu=present:NoSchedule) so that GPU-requesting pods can
// actually land there — non-GPU workloads are meant to stay off those nodes, but a pod that
// legitimately requests GPUs must be able to schedule onto one. This is purely an
// execution-engine concern the agent never declares in JobSpec: any request with
// gpuCount > 0 gets it automatically.
func gpuTolerations(gpuCount int, taintKey string) []corev1.Toleration {
	if gpuCount <= 0 {
		return nil
	}
	return []corev1.Toleration{{
		Key:      taintKey,
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	}}
}

// gpuNodeAffinity translates a JobSpec's GPU hardware requirement into a native nodeAffinity
// over the well-known nvidia.com/gpu.product node label (set by the NVIDIA GPU Feature
// Discovery add-on), using the operator's GPUType->product-label mapping (nodeLabelByType,
// from openresearch.yaml). The acceptable set is AcceptableGPUTypes if the agent gave one
// (interchangeable scheduling), otherwise just [gpuType] — a plain gpu_type with no
// acceptable_gpu_types must still land on hardware of exactly that type: the extended
// resource request (nvidia.com/gpu quantity) alone only guarantees *a* GPU node, never *the
// requested model*, since every GPU type advertises the same resource name. Returns nil only
// when the cluster has no node-label mapping configured at all (e.g. GPU-less dev clusters
// substituting "cpu"), so a plain resource request is all that's meaningful there.
// gpuNodeAffinity builds the nodeAffinity for gpuType (or, if set, the OR of acceptable —
// GPU-type interchangeability). The node-label KEY used is always gpuType's own
// (nodeLabelKeyFor) — acceptable entries whose vendor plumbing uses a *different* label key
// are silently skipped, since mixing vendors within one job's acceptable set is incoherent
// (the k8s resource request itself is one specific extended-resource name; you can't ask for
// "amd.com/gpu OR nvidia.com/gpu" in a single container). In practice this means
// acceptable_gpu_types should only ever list types from the same vendor as gpu_type.
func (c *JobWorkloadClient) gpuNodeAffinity(gpuType domain.GPUType, acceptable []domain.GPUType) *corev1.NodeAffinity {
	if len(c.nodeLabelByType) == 0 || gpuType == "" {
		return nil
	}
	labelKey := c.nodeLabelKeyFor(gpuType)
	wanted := acceptable
	if len(wanted) == 0 {
		wanted = []domain.GPUType{gpuType}
	}
	values := make([]string, 0, len(wanted))
	for _, t := range wanted {
		if c.nodeLabelKeyFor(t) != labelKey {
			continue // different vendor's label key — can't OR them into one nodeAffinity term
		}
		if v, ok := c.nodeLabelByType[string(t)]; ok && v != "" {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return nil
	}
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      labelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   values,
				}},
			}},
		},
	}
}

func jobName(experimentID string) string {
	name := "exp-" + experimentID
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	v := strings.ToLower(strings.Trim(b.String(), "-."))
	if len(v) > 63 {
		v = v[:63]
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func buildRestConfig(kubeconfigPath, context string) (*rest.Config, error) {
	if kubeconfigPath == "" && context == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	} else if home := homedir.HomeDir(); home != "" {
		loadingRules.Precedence = append(loadingRules.Precedence, filepath.Join(home, ".kube", "config"))
	}
	overrides := &clientcmd.ConfigOverrides{}
	if context != "" {
		overrides.CurrentContext = context
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

func traceparentFromID(id string) string {
	h := sha256.Sum256([]byte(id))
	return fmt.Sprintf("00-%x-%x-01", h[:16], h[16:24])
}
