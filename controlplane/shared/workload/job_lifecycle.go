package workload

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

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
// cores across schedulable nodes, and the same minus every non-terminal, already-scheduled
// pod's CPU requests (not just openresearch-managed pods — a true "how much is actually free"
// number, the same thing a real scheduler would compute). Replaces the old static-config-only
// capacity model for CPU-only jobs; pushed to the control plane on every desired-state poll.
//
// Only counts a pod's request against capacity once it is actually assigned to a node
// (p.Spec.NodeName != "") — an unassigned/Pending pod's request is NOT subtracted here (this
// used to blanket-subtract it cluster-wide regardless of whether/where it would ever land,
// which is both imprecise and redundant: the control plane's own durable pending-capacity
// reservation, Postgres pending_capacity_reservations, already accounts for a job's footprint
// in the exact window between admission and its pod actually being scheduled — see
// loop_tick.go step 2b). This collector's job is only to report the live, already-scheduled
// truth. See SCHEDULING_GENERALIZATION_PLAN.md's Class B step 2 correctness fix, applied here
// to this pre-existing CPU collector too, not just the new RAM/storage ones below.
func (c *JobWorkloadClient) GetLiveCPUCapacity(ctx context.Context) (available, total float64, err error) {
	nodes, pods, err := c.listSchedulableNodesAndPods(ctx)
	if err != nil {
		return 0, 0, err
	}
	totalMilli, requestedMilli := sumAllocatableAndRequested(nodes, pods, corev1.ResourceCPU, func(q resource.Quantity) int64 { return q.MilliValue() })
	availMilli := totalMilli - requestedMilli
	if availMilli < 0 {
		availMilli = 0
	}
	return float64(availMilli) / 1000.0, float64(totalMilli) / 1000.0, nil
}

// GetLiveRAMCapacity/GetLiveStorageCapacity mirror GetLiveCPUCapacity for the two Class B
// (hard-cap, no billing) dimensions — memory and ephemeral-storage — reported in bytes. Same
// accounting: total allocatable across schedulable nodes minus already-scheduled non-terminal
// pods' requests, counted only against the node a pod is actually assigned to.
func (c *JobWorkloadClient) GetLiveRAMCapacity(ctx context.Context) (available, total int64, err error) {
	nodes, pods, err := c.listSchedulableNodesAndPods(ctx)
	if err != nil {
		return 0, 0, err
	}
	totalBytes, requestedBytes := sumAllocatableAndRequested(nodes, pods, corev1.ResourceMemory, func(q resource.Quantity) int64 { return q.Value() })
	avail := totalBytes - requestedBytes
	if avail < 0 {
		avail = 0
	}
	return avail, totalBytes, nil
}

func (c *JobWorkloadClient) GetLiveStorageCapacity(ctx context.Context) (available, total int64, err error) {
	nodes, pods, err := c.listSchedulableNodesAndPods(ctx)
	if err != nil {
		return 0, 0, err
	}
	totalBytes, requestedBytes := sumAllocatableAndRequested(nodes, pods, corev1.ResourceEphemeralStorage, func(q resource.Quantity) int64 { return q.Value() })
	avail := totalBytes - requestedBytes
	if avail < 0 {
		avail = 0
	}
	return avail, totalBytes, nil
}

// listSchedulableNodesAndPods is the shared node/pod fetch behind every live capacity
// collector above, so they all read the same consistent-enough snapshot per call.
func (c *JobWorkloadClient) listSchedulableNodesAndPods(ctx context.Context) ([]corev1.Node, []corev1.Pod, error) {
	nodeList, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("workload: list nodes: %w", err)
	}
	podList, err := c.kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("workload: list pods: %w", err)
	}
	return nodeList.Items, podList.Items, nil
}

// sumAllocatableAndRequested totals resourceName's allocatable quantity across schedulable
// nodes, and separately totals it across every non-terminal pod's container requests — but,
// per GetLiveCPUCapacity's doc comment, ONLY for pods already assigned to a node
// (p.Spec.NodeName != ""); an unassigned/Pending pod contributes nothing here.
func sumAllocatableAndRequested(nodes []corev1.Node, pods []corev1.Pod, resourceName corev1.ResourceName, extract func(resource.Quantity) int64) (total, requested int64) {
	for _, n := range nodes {
		if n.Spec.Unschedulable {
			continue
		}
		if q, ok := n.Status.Allocatable[resourceName]; ok {
			total += extract(q)
		}
	}
	for _, p := range pods {
		if p.Spec.NodeName == "" {
			continue // not yet scheduled — covered by pending_capacity_reservations instead
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, ctr := range p.Spec.Containers {
			if q, ok := ctr.Resources.Requests[resourceName]; ok {
				requested += extract(q)
			}
		}
	}
	return total, requested
}

// GetLiveGPUCapacity returns this cluster's real, current GPU capacity per flavor: total
// allocatable extended-resource quantity across schedulable nodes carrying that GPU type's own
// node label, and the same minus every non-terminal pod's request for that resource name,
// counted only across those labeled nodes. Mirrors GetLiveCPUCapacity's allocatable-minus-
// requested model, keyed by flavor so it slots directly into the same guaranteed/burst maps CPU
// capacity uses. Flavors with no node-label mapping configured are omitted (nothing to count).
func (c *JobWorkloadClient) GetLiveGPUCapacity(ctx context.Context) (available, total map[string]int64, err error) {
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("workload: list nodes: %w", err)
	}
	pods, err := c.kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("workload: list pods: %w", err)
	}

	available = make(map[string]int64)
	total = make(map[string]int64)
	for flavor, gpuName := range c.nameByFlavor() {
		labelValue := c.nodeLabelByType[gpuName]
		if labelValue == "" {
			continue
		}
		labelKey := c.nodeLabelKeyFor(domain.GPUType(gpuName))
		resourceName := corev1.ResourceName(c.resourceNameFor(domain.GPUType(gpuName)))

		var totalQty int64
		onFlavorNode := make(map[string]bool)
		for _, n := range nodes.Items {
			if n.Spec.Unschedulable || n.Labels[labelKey] != labelValue {
				continue
			}
			if q, ok := n.Status.Allocatable[resourceName]; ok {
				totalQty += q.Value()
			}
			onFlavorNode[n.Name] = true
		}

		var requestedQty int64
		for _, p := range pods.Items {
			if !onFlavorNode[p.Spec.NodeName] {
				continue
			}
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			for _, ctr := range p.Spec.Containers {
				if q, ok := ctr.Resources.Requests[resourceName]; ok {
					requestedQty += q.Value()
				}
			}
		}

		availQty := totalQty - requestedQty
		if availQty < 0 {
			availQty = 0
		}
		available[flavor] = availQty
		total[flavor] = totalQty
	}
	return available, total, nil
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

// GetFlavorCapacity returns nominal GPU + live CPU/RAM/storage capacity as a canonical
// domain.Footprint. demo: GPU is not reading live reservation state from the cluster (nominal
// config only); CPU/RAM/storage use the same live allocatable-minus-requested numbers
// GetLiveCPUCapacity/GetLiveRAMCapacity/GetLiveStorageCapacity compute, so a mixed job's joint
// fit can be checked against one vector.
func (c *JobWorkloadClient) GetFlavorCapacity(ctx context.Context) (guaranteed, burst domain.Footprint, err error) {
	nominal := c.gpuNominalCapacity()
	cpuAvail, _, err := c.GetLiveCPUCapacity(ctx)
	if err != nil {
		return nil, nil, err
	}
	ramAvail, _, err := c.GetLiveRAMCapacity(ctx)
	if err != nil {
		return nil, nil, err
	}
	storageAvail, _, err := c.GetLiveStorageCapacity(ctx)
	if err != nil {
		return nil, nil, err
	}
	fp := domain.CapacityFootprint(cpuAvail, nominal, ramAvail, storageAvail)
	// Guaranteed and burst share the same physical pool here, same as queuebackend.Backend —
	// preemption enforces the tier boundary, not a capacity split.
	return fp, fp, nil
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

// GetAdmittedGPUType reports which GPU type this experiment's Job actually landed on. When
// AcceptableGPUTypes lists more than one flavor, the k8s scheduler — not this client — picks the
// node, so the only way to know which flavor was chosen is to read it back from the live cluster
// (see ResolveAdmittedGPUType). Falls back to the originally requested exp.GPUType if no pod is
// scheduled yet or the resolve fails — the same "not known yet" default the caller already
// expects before any report has arrived.
func (c *JobWorkloadClient) GetAdmittedGPUType(ctx context.Context, exp *domain.Experiment) domain.GPUType {
	resolved, _, err := c.ResolveAdmittedGPUType(ctx, exp.ID)
	if err != nil || resolved == "" {
		return exp.GPUType
	}
	return resolved
}

// ResolveAdmittedGPUType inspects experimentID's currently-scheduled pod(s) and returns the GPU
// type actually admitted, reverse-mapping the assigned node's GPU product label back to a
// configured type name via nodeLabelByType/nodeLabelKeyByType (gpuNodeAffinity's mapping, in
// reverse). This is the only way to observe which acceptable_gpu_types flavor the k8s scheduler
// actually chose — neither the Job spec nor this client's own admission decision records it.
//
// Resolves from rank 0's node (batch.kubernetes.io/job-completion-index=0) when available,
// falling back to the first scheduled pod found — a deliberately simple choice for distributed
// jobs rather than full cross-rank reconciliation. consistent=false flags (for the caller to log;
// this alone never blocks reporting) that another already-scheduled rank landed on a different
// GPU type than rank 0, which callers should treat as an error condition.
//
// Returns ("", true, nil) if no pod is scheduled onto a node yet, or the scheduled node carries
// none of the configured GPU labels (non-GPU/dev cluster) — both "nothing to report yet", not an
// error.
func (c *JobWorkloadClient) ResolveAdmittedGPUType(ctx context.Context, experimentID string) (gpuType domain.GPUType, consistent bool, err error) {
	pods, err := c.kube.CoreV1().Pods(OpenResearchNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("openresearch.io/experiment-id=%s", experimentID),
	})
	if err != nil {
		return "", true, fmt.Errorf("workload: list pods for %s: %w", experimentID, err)
	}

	var primary *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" {
			continue
		}
		if primary == nil {
			primary = p
		}
		if p.Labels["batch.kubernetes.io/job-completion-index"] == "0" {
			primary = p
			break
		}
	}
	if primary == nil {
		return "", true, nil
	}

	node, err := c.kube.CoreV1().Nodes().Get(ctx, primary.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return "", true, fmt.Errorf("workload: get node %s: %w", primary.Spec.NodeName, err)
	}
	gpuType = c.gpuTypeFromNodeLabels(node.Labels)
	if gpuType == "" {
		return "", true, nil
	}

	consistent = true
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" || p.Spec.NodeName == primary.Spec.NodeName {
			continue
		}
		n, getErr := c.kube.CoreV1().Nodes().Get(ctx, p.Spec.NodeName, metav1.GetOptions{})
		if getErr != nil {
			continue
		}
		if t := c.gpuTypeFromNodeLabels(n.Labels); t != "" && t != gpuType {
			consistent = false
		}
	}
	return gpuType, consistent, nil
}

// gpuTypeFromNodeLabels reverse-maps a node's own labels back to the configured GPU type whose
// nodeLabelByType/nodeLabelKeyByType entry matches — the inverse of gpuNodeAffinity's
// type-to-label translation. Returns "" if no configured type matches (unlabeled/non-GPU node).
func (c *JobWorkloadClient) gpuTypeFromNodeLabels(labels map[string]string) domain.GPUType {
	for typeName, labelValue := range c.nodeLabelByType {
		if labelValue == "" {
			continue
		}
		key := c.nodeLabelKeyFor(domain.GPUType(typeName))
		if labels[key] == labelValue {
			return domain.GPUType(typeName)
		}
	}
	return ""
}

func (c *JobWorkloadClient) ProvisionAgent(_ context.Context, _ string) error { return nil }
