package k8sexec

import (
	"context"
	stderrors "errors"
	"fmt"
	"log"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/workloadkeys"
)

func (c *JobWorkloadClient) CreateWorkload(ctx context.Context, exp *domain.Experiment) error {
	if err := c.ensureNamespace(ctx, HypothesisLoopNamespace); err != nil {
		return err
	}
	// No admission handshake here: the scheduler loop already decided this experiment fits
	// available capacity before calling CreateWorkload, so the Job is created ready-to-run.
	placement, err := c.resolvePlacementFor(ctx, exp)
	if err != nil {
		return err
	}
	job, err := c.BuildJob(exp, placement)
	if err != nil {
		return err
	}
	if job.Spec.Template.Spec.Subdomain != "" {
		if err := c.ensureHeadlessService(ctx, job); err != nil {
			return fmt.Errorf("workload: ensure headless service: %w", err)
		}
	}
	if len(job.Spec.Template.Spec.ResourceClaims) > 0 {
		// Must exist before the Job's pod is created (async, via the Job controller) or the
		// pod fails to schedule — same reason ensureHeadlessService runs before Job Create.
		if err := c.ensureResourceClaimTemplate(ctx, job, exp); err != nil {
			return fmt.Errorf("workload: ensure resource claim template: %w", err)
		}
	}
	_, err = c.kube.BatchV1().Jobs(HypothesisLoopNamespace).Create(ctx, job, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		// Job names are deterministic per experiment ID, so re-admission after preemption
		// collides with the prior Job object if WaitForJobDeletion's background-propagation
		// delete hasn't removed it yet — observed live as exp stuck SUBMITTED for 3+min
		// because this branch treated the stale, still-terminating object as a safe no-op.
		// A DeletionTimestamp means it's the old, dying object: surface an error so submitJob
		// rolls back to QUEUED and retries once the delete actually completes.
		existing, getErr := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).Get(ctx, job.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("workload: inspect existing job %s: %w", job.Name, getErr)
		}
		if existingJobMatchesDesired(existing, job, exp.ID) {
			return nil
		}
		if existing.DeletionTimestamp == nil {
			if deleteErr := c.DeleteWorkload(ctx, exp.ID); deleteErr != nil {
				return fmt.Errorf("workload: delete mismatched existing job %s: %w", job.Name, deleteErr)
			}
		}
		return fmt.Errorf("workload: stale job %s removed or still terminating; retry creation", job.Name)
	}
	return err
}

func existingJobMatchesDesired(existing, desired *batchv1.Job, experimentID string) bool {
	return existing != nil && desired != nil && existing.DeletionTimestamp == nil &&
		existing.Annotations[DesiredSpecHashAnnotation] == desired.Annotations[DesiredSpecHashAnnotation] &&
		existing.Labels[workloadkeys.ManagedBy] == workloadkeys.ManagedByValue &&
		existing.Labels[workloadkeys.ExperimentID] == experimentID
}

// ensureHeadlessService creates the headless (ClusterIP: None) Service a distributed job's
// pods need for rank 0's stable DNS name (see HYPOTHESISLOOP_MASTER_ADDR in BuildJob) — named
// identically to the Job and selecting its pods by experiment-id label.
func (c *JobWorkloadClient) ensureHeadlessService(ctx context.Context, job *batchv1.Job) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: HypothesisLoopNamespace,
			Labels:    job.Labels,
			Annotations: map[string]string{
				DesiredSpecHashAnnotation: job.Annotations[DesiredSpecHashAnnotation],
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  job.Spec.Template.Labels,
		},
	}
	_, err := c.kube.CoreV1().Services(HypothesisLoopNamespace).Create(ctx, svc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existing, getErr := c.kube.CoreV1().Services(HypothesisLoopNamespace).Get(ctx, svc.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if existing.Annotations[DesiredSpecHashAnnotation] == job.Annotations[DesiredSpecHashAnnotation] && existing.DeletionTimestamp == nil &&
			existing.Labels[workloadkeys.ManagedBy] == workloadkeys.ManagedByValue && existing.Labels[workloadkeys.ExperimentID] == job.Labels[workloadkeys.ExperimentID] {
			return nil
		}
		if deleteErr := c.kube.CoreV1().Services(HypothesisLoopNamespace).Delete(ctx, svc.Name, metav1.DeleteOptions{}); deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete drifted headless Service: %w", deleteErr)
		}
		return fmt.Errorf("drifted headless Service %s removed; retry creation", svc.Name)
	}
	return err
}

// ListManagedJobs returns the experiment IDs of every Job this client manages in the current
// cluster — used by cluster-agent to discover what actually exists, to diff against desired state.
func (c *JobWorkloadClient) ListManagedJobs(ctx context.Context) ([]string, error) {
	jobs, err := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", workloadkeys.ManagedBy, workloadkeys.ManagedByValue),
	})
	if err != nil {
		return nil, fmt.Errorf("workload: list managed jobs: %w", err)
	}
	ids := make([]string, 0, len(jobs.Items))
	for _, j := range jobs.Items {
		// A Job with a DeletionTimestamp is mid-teardown (Background propagation leaves it
		// listable while pods are still removed) — the dying generation of a
		// preempted-then-recreated experiment. Counting it as "actual" would make reconcile
		// silently skip (re)creating the new generation until it finally vanishes from List().
		if j.DeletionTimestamp != nil {
			continue
		}
		id := j.Labels[workloadkeys.ExperimentID]
		if id == "" {
			log.Printf("workload: skipping managed job %q with no experiment identity", j.Name)
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ListManagedJobsForStatus returns the experiment IDs of every managed Job, including ones that
// are still terminating. Implements agentloop.StatusLister.
//
// Deliberately a different set from ListManagedJobs. A status push is a complete cluster
// snapshot, and the control plane reads an experiment missing from one as its workload having
// vanished. A drift-delete-then-recreate leaves the Job terminating for a moment, and reporting
// that as "not here" told the control plane real, still-running training had disappeared. The
// honest report is that the Job is present and not yet Running — which is what PollJobPhaseAndUID
// returns for it once its pods are gone, so no phase mapping is needed here, only the inclusion.
func (c *JobWorkloadClient) ListManagedJobsForStatus(ctx context.Context) ([]string, error) {
	jobs, err := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", workloadkeys.ManagedBy, workloadkeys.ManagedByValue),
	})
	if err != nil {
		return nil, fmt.Errorf("workload: list managed jobs for status: %w", err)
	}
	ids := make([]string, 0, len(jobs.Items))
	for _, j := range jobs.Items {
		id := j.Labels[workloadkeys.ExperimentID]
		if id == "" {
			// One unidentifiable object must not abort the pass: this list drives every create
			// and delete, so failing it wedged reconcile for the whole cluster until a human
			// intervened (important.md #19).
			log.Printf("workload: skipping managed job %q with no experiment identity", j.Name)
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// WorkloadMatchesDesired compares the current Job with a fresh compilation of desired state.
// Kubernetes-defaulted fields never enter the comparison since the pre-create desired hash is
// stored as an annotation.
func (c *JobWorkloadClient) WorkloadMatchesDesired(ctx context.Context, exp *domain.Experiment) (bool, error) {
	placement, err := c.resolvePlacementFor(ctx, exp)
	if err != nil {
		return false, err
	}
	desired, err := c.BuildJob(exp, placement)
	if err != nil {
		return false, err
	}
	actual, err := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("workload: get desired-hash Job: %w", err)
	}
	if actual.Annotations[DesiredSpecHashAnnotation] != desired.Annotations[DesiredSpecHashAnnotation] ||
		actual.Labels[workloadkeys.ManagedBy] != workloadkeys.ManagedByValue || actual.Labels[workloadkeys.ExperimentID] != exp.ID {
		return false, nil
	}
	desiredHash := desired.Annotations[DesiredSpecHashAnnotation]
	service, serviceErr := c.kube.CoreV1().Services(HypothesisLoopNamespace).Get(ctx, desired.Name, metav1.GetOptions{})
	wantsService := desired.Spec.Template.Spec.Subdomain != ""
	if serviceErr != nil && !errors.IsNotFound(serviceErr) {
		return false, fmt.Errorf("workload: get auxiliary Service: %w", serviceErr)
	}
	if wantsService != (serviceErr == nil) || (serviceErr == nil && (service.Annotations[DesiredSpecHashAnnotation] != desiredHash ||
		service.Labels[workloadkeys.ManagedBy] != workloadkeys.ManagedByValue || service.Labels[workloadkeys.ExperimentID] != exp.ID)) {
		return false, nil
	}

	wantsTemplate := len(desired.Spec.Template.Spec.ResourceClaims) > 0
	draMatches, err := c.draTemplateMatches(ctx, desired.Name, exp.ID, wantsTemplate, desiredHash)
	if err != nil {
		return false, fmt.Errorf("workload: compare auxiliary ResourceClaimTemplate: %w", err)
	}
	if !draMatches {
		return false, nil
	}
	return true, nil
}

// ListManagedAuxiliaryWorkloads returns experiment IDs referenced by managed headless Services
// and DRA ResourceClaimTemplates. Used only to remove orphans; auxiliary existence must never
// stand in for Job existence when deciding whether to create.
func (c *JobWorkloadClient) ListManagedAuxiliaryWorkloads(ctx context.Context) ([]string, error) {
	ids := map[string]struct{}{}
	services, err := c.kube.CoreV1().Services(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", workloadkeys.ManagedBy, workloadkeys.ManagedByValue),
	})
	if err != nil {
		return nil, fmt.Errorf("workload: list managed services: %w", err)
	}
	for _, service := range services.Items {
		id := service.Labels[workloadkeys.ExperimentID]
		if id == "" {
			log.Printf("workload: skipping managed Service %q with no experiment identity", service.Name)
			continue
		}
		ids[id] = struct{}{}
	}
	if err := c.addManagedDRAIDs(ctx, ids); err != nil {
		return nil, fmt.Errorf("workload: list managed resource claim templates: %w", err)
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	return out, nil
}

// GetLiveCPUCapacity returns this cluster's real, current CPU-core capacity: total allocatable
// cores across schedulable nodes, minus every non-terminal, already-scheduled pod's CPU
// requests (all pods, not just hypothesisloop-managed ones) — a true "how much is actually free"
// number, pushed during every control-plane reconcile exchange.
//
// Only counts a pod's request once it's assigned to a node (p.Spec.NodeName != ""); an
// unassigned/Pending pod contributes nothing, since the control plane already derives capacity
// from nominal totals minus its complete PostgreSQL desired footprint. This collector only
// reports live actual state.
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

// GetLiveRAMCapacity/GetLiveStorageCapacity mirror GetLiveCPUCapacity for the two hard-cap,
// no-billing dimensions — memory and ephemeral-storage — reported in bytes.
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
// collector, so they all read the same consistent-enough snapshot per call — and apply one
// definition of "schedulable", rather than each collector deciding for itself.
//
// A node counts only when it is both uncordoned and Ready. Ready matters as much as the cordon
// flag: a node whose machine is powered off (or whose kubelet has died, or that lost its network)
// stays in the API as a registered NotReady object, still advertising its full Allocatable. Left
// unfiltered, that hardware keeps being reported as live capacity long after it stopped existing,
// and jobs admitted against it queue forever with nothing to run them.
func (c *JobWorkloadClient) listSchedulableNodesAndPods(ctx context.Context) ([]corev1.Node, []corev1.Pod, error) {
	nodeList, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("workload: list nodes: %w", err)
	}
	podList, err := c.kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("workload: list pods: %w", err)
	}
	nodes := make([]corev1.Node, 0, len(nodeList.Items))
	for _, n := range nodeList.Items {
		if schedulableNode(n) {
			nodes = append(nodes, n)
		}
	}
	return nodes, podList.Items, nil
}

// schedulableNode reports whether n can actually accept work right now — see
// listSchedulableNodesAndPods for why NotReady must disqualify a node, not just a cordon.
func schedulableNode(n corev1.Node) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// sumAllocatableAndRequested totals resourceName's allocatable quantity across schedulable
// nodes, and separately across every non-terminal pod's container requests — only for pods
// already assigned to a node (see GetLiveCPUCapacity doc).
func sumAllocatableAndRequested(nodes []corev1.Node, pods []corev1.Pod, resourceName corev1.ResourceName, extract func(resource.Quantity) int64) (total, requested int64) {
	for _, n := range nodes {
		if q, ok := n.Status.Allocatable[resourceName]; ok {
			total += extract(q)
		}
	}
	for _, p := range pods {
		if p.Spec.NodeName == "" {
			continue // actual usage starts only once Kubernetes assigns a node
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

// GetLiveAcceleratorCapacitySnapshot returns aggregate and per-node actual accelerator state
// from one Kubernetes node/pod listing (DRA inventory listed once per configured driver), so
// the reconcile exchange reports one internally consistent snapshot without duplicate reads.
func (c *JobWorkloadClient) GetLiveAcceleratorCapacitySnapshot(ctx context.Context) (available, total map[string]int64, byNode map[string]map[string]int64, nodeLabels map[string]map[string]string, err error) {
	// Hardware publishes many true facts about itself — a Tenstorrent card reports its arch, its
	// board type, its tray, its serial. Each is a legitimate accelerator type, but reporting all
	// of them would bury the two an operator actually cares about under twenty per-serial
	// entries. The priced catalog is the filter: it is precisely the set of types that can be
	// billed, and admission already refuses anything unpriced, so an unpriced type could never
	// have been submitted anyway. The operator prices the granularity they want; that is exactly
	// what capacity reports.
	priced := c.pricedAcceleratorTypes
	nodes, pods, err := c.listSchedulableNodesAndPods(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	requested := make(map[string]map[corev1.ResourceName]int64)
	for _, pod := range pods {
		if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if requested[pod.Spec.NodeName] == nil {
			requested[pod.Spec.NodeName] = make(map[corev1.ResourceName]int64)
		}
		for _, ctr := range pod.Spec.Containers {
			for name, qty := range ctr.Resources.Requests {
				requested[pod.Spec.NodeName][name] += qty.Value()
			}
		}
	}

	available = make(map[string]int64)
	total = make(map[string]int64)
	byNode = make(map[string]map[string]int64)
	nodeLabels = make(map[string]map[string]string)
	draSnapshots, err := c.liveDRACapacitySnapshots(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for acceleratorName, snapshot := range draSnapshots {
		key := acceleratorName
		if !priced[strings.ToLower(key)] {
			continue
		}
		available[key], total[key] = snapshot.available, snapshot.total
		for node, free := range snapshot.freeByNode {
			if byNode[node] == nil {
				byNode[node] = make(map[string]int64)
			}
			byNode[node][key] = free
		}
	}
	// Device-plugin hardware: a node's extended resource says how many devices it has, and the
	// vendor's own node labels say what they are. Capacity is keyed by those labels — the same
	// "key=value" a job spec names — so an H100 node and an L40 node are separate types instead
	// of one undifferentiated nvidia.com/gpu pool that matches no submittable accelerator_type.
	//
	// A device is counted under every label in its vendor's domain, for the same reason DRA
	// devices are counted under every attribute: these are selectors a job picks one of, not a
	// partition, and it keeps the platform from having to decide which label is the model name.
	for _, node := range nodes {
		nodeLabels[node.Name] = node.Labels
		for resourceName, allocatable := range node.Status.Allocatable {
			name := string(resourceName)
			if !strings.Contains(name, "/") || strings.HasPrefix(name, "kubernetes.io/") {
				continue
			}
			free := allocatable.Value() - requested[node.Name][resourceName]
			if free < 0 {
				free = 0
			}
			domainName := resourceDomain(name)
			for labelKey, labelValue := range node.Labels {
				if resourceDomain(labelKey) != domainName {
					continue
				}
				key := labelKey + "=" + labelValue
				if !priced[strings.ToLower(key)] {
					continue
				}
				if byNode[node.Name] == nil {
					byNode[node.Name] = make(map[string]int64)
				}
				byNode[node.Name][key] = free
				total[key] += allocatable.Value()
				available[key] += free
			}
		}
	}
	return available, total, byNode, nodeLabels, nil
}

func (c *JobWorkloadClient) DeleteWorkload(ctx context.Context, experimentID string) error {
	prop := metav1.DeletePropagationBackground
	jobErr := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).Delete(ctx, jobName(experimentID), metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
	if errors.IsNotFound(jobErr) {
		jobErr = nil
	}
	// The headless Service (see ensureHeadlessService) is only created for distributed jobs,
	// but deleting it unconditionally is harmless (NotFound swallowed) and avoids needing to
	// know NumNodes at this call site.
	svcErr := c.kube.CoreV1().Services(HypothesisLoopNamespace).Delete(ctx, jobName(experimentID), metav1.DeleteOptions{})
	if errors.IsNotFound(svcErr) {
		svcErr = nil
	}
	// Same unconditional-attempt/NotFound-swallowed pattern as the headless Service: only
	// DRA-mode jobs created a template, but deleting unconditionally avoids needing to know
	// allocation_mode here. The kubelet-derived ResourceClaim(s) are owned by the pod and
	// cleaned up natively by k8s; this only removes the template object itself.
	claimErr := c.deleteDRAResource(ctx, experimentID)
	return stderrors.Join(jobErr, svcErr, claimErr)
}

func (c *JobWorkloadClient) WaitForJobDeletion(ctx context.Context, experimentID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	name := jobName(experimentID)
	for time.Now().Before(deadline) {
		_, err := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).Get(ctx, name, metav1.GetOptions{})
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

func (c *JobWorkloadClient) PollJobPhase(ctx context.Context, experimentID string) (workload.JobPhase, error) {
	phase, _, err := c.PollJobPhaseAndUID(ctx, experimentID)
	return phase, err
}

// PollJobPhaseAndUID fetches the k8s Job once and returns both its phase and UID ("" if the
// Job doesn't exist) — one API call instead of two separate Get()s. This isn't just an
// efficiency cleanup: two polls a few hundred ms apart could each observe "Active>0" across a
// preempt-then-recreate cycle, making an old Job's disappearance and a new Job's appearance
// look like "no change" to a phase-only comparison. Comparing UID alongside phase (see
// cluster-agent's reportChangedStatuses) catches that, since UID always changes on delete+recreate.
func (c *JobWorkloadClient) PollJobPhaseAndUID(ctx context.Context, experimentID string) (workload.JobPhase, string, error) {
	job, err := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).Get(ctx, jobName(experimentID), metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return workload.JobPhaseGone, "", nil
		}
		return workload.JobPhasePending, "", err
	}
	uid := string(job.UID)
	// Check the native JobComplete condition before falling back to a raw Succeeded count:
	// for Indexed jobs with a SuccessPolicy, a non-master worker finishing first bumps
	// Status.Succeeded without the job actually being done.
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return workload.JobPhaseSucceeded, uid, nil
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return workload.JobPhaseFailed, uid, nil
		}
	}
	if job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != batchv1.IndexedCompletion {
		if job.Status.Succeeded > 0 {
			return workload.JobPhaseSucceeded, uid, nil
		}
	}
	// Ready, not Active. Active counts a pod from the moment it exists, so a pod sitting in
	// ImagePullBackOff or ContainerCreating reported as Running — which told the control plane a
	// job was executing (and consuming, and therefore billable) before its container had ever
	// started, and routed it away from the pending path that would have diagnosed it.
	//
	// Ready is the count of pods whose containers are actually up. These pods define no readiness
	// probe (nothing in BuildJob sets one), so for them the Ready condition means exactly
	// "the containers are running" and nothing about service availability.
	//
	// A nil Ready is a cluster that does not populate the field at all: no evidence of execution,
	// so the job is reported as not yet running rather than assumed to be.
	if job.Status.Ready != nil && *job.Status.Ready > 0 {
		return workload.JobPhaseRunning, uid, nil
	}
	return workload.JobPhasePending, uid, nil
}

// GetAdmittedAcceleratorType reports which accelerator type this experiment's Job actually landed on. When
// AcceptableAcceleratorTypes lists more than one flavor, the k8s scheduler picks the node, so
// the only way to know which was chosen is to read it back from the live cluster (see
// ResolveAdmittedAcceleratorType). Missing actual placement is an error; callers retry.
func (c *JobWorkloadClient) GetAdmittedAcceleratorType(ctx context.Context, exp *domain.Experiment) (domain.AcceleratorType, error) {
	resolved, _, _, err := c.ResolveAdmittedAcceleratorType(ctx, exp.ID)
	if err != nil {
		return "", err
	}
	if resolved == "" {
		return "", fmt.Errorf("workload: accelerator type for %s is not observed yet", exp.ID)
	}
	return resolved, nil
}

// ResolveAdmittedAcceleratorType inspects experimentID's currently-scheduled pod(s) and returns the accelerator
// type actually admitted, reverse-mapping the assigned node's accelerator product label back to
// a configured type name (acceleratorNodeAffinity's mapping, in reverse). This is the only way
// to observe which acceptable_accelerator_types flavor the k8s scheduler actually chose.
//
// node is rank 0's k8s node name (or the first scheduled pod's, if rank 0 isn't up yet) —
// piggybacked on this lookup since the only consumer (cluster-agent's status report loop)
// always wants both together. This is the sole source of job->physical-node attribution in the
// system; PushStatus forwards it straight into the metrics store (metricsdb.RecordExperimentNode),
// per this repo's "metrics only in the metrics store, no duplicates" rule.
//
// Resolves from rank 0's node when available, falling back to the first scheduled pod found —
// a deliberately simple choice rather than full cross-rank reconciliation. consistent=false
// flags (for the caller to log only) that another rank landed on a different accelerator type.
//
// Returns ("", "", true, nil) if no pod is scheduled yet. node can be non-empty even when
// acceleratorType is "" (e.g. a non-accelerator/dev cluster) — reported independently since
// node attribution is meaningful even without a recognized accelerator type.
func (c *JobWorkloadClient) ResolveAdmittedAcceleratorType(ctx context.Context, experimentID string) (acceleratorType domain.AcceleratorType, node string, consistent bool, err error) {
	pods, err := c.kube.CoreV1().Pods(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", workloadkeys.ExperimentID, experimentID),
	})
	if err != nil {
		return "", "", true, fmt.Errorf("workload: list pods for %s: %w", experimentID, err)
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
		return "", "", true, nil
	}
	node = primary.Spec.NodeName

	acceleratorType = domain.AcceleratorType(primary.Annotations[AcceleratorTypeAnnotation])
	if acceleratorType == "" {
		return "", node, true, nil
	}

	consistent = true
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" {
			continue
		}
		if t := domain.AcceleratorType(p.Annotations[AcceleratorTypeAnnotation]); t != acceleratorType {
			consistent = false
		}
	}
	return acceleratorType, node, consistent, nil
}

// acceleratorTypeFromNodeLabels reverse-maps a node's labels back to the configured accelerator
// type — the inverse of acceleratorNodeAffinity's type-to-label translation. Returns "" if no
// configured type matches.
func (c *JobWorkloadClient) ProvisionAgent(_ context.Context, _ string) error { return nil }
