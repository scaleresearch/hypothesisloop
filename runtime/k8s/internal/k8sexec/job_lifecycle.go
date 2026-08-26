package k8sexec

import (
	"context"
	stderrors "errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
	"github.com/scaleresearch/hypothesisloop/runtime/shared/agentexec"
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
	jobs, err := c.BuildJobs(exp, placement)
	if err != nil {
		return err
	}
	// The Service first, and once for the whole experiment however many groups it has: a pod
	// that starts before the name its peers are told to dial exists resolves nothing and dies in
	// the rendezvous.
	if jobs[0].Spec.Template.Spec.Subdomain != "" {
		if err := c.ensureHeadlessService(ctx, exp, jobs[0]); err != nil {
			return fmt.Errorf("workload: ensure headless service: %w", err)
		}
	}
	for _, job := range jobs {
		if err := c.createJob(ctx, exp, job); err != nil {
			return err
		}
	}
	return nil
}

// createJob creates one compiled Job, resolving a collision with a previous generation of the
// same name. Split out because a grouped experiment creates several and each one meets the same
// stale-object question independently.
func (c *JobWorkloadClient) createJob(ctx context.Context, exp *domain.Experiment, job *batchv1.Job) error {
	if len(job.Spec.Template.Spec.ResourceClaims) > 0 {
		// Must exist before the Job's pod is created (async, via the Job controller) or the
		// pod fails to schedule — same reason ensureHeadlessService runs before Job Create.
		if err := c.ensureResourceClaimTemplate(ctx, job, exp); err != nil {
			return fmt.Errorf("workload: ensure resource claim template: %w", err)
		}
	}
	_, err := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).Create(ctx, job, metav1.CreateOptions{})
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
			if deleteErr := c.DeleteWorkload(ctx, exp.ID, false); deleteErr != nil {
				return fmt.Errorf("workload: delete mismatched existing job %s: %w", job.Name, deleteErr)
			}
		}
		return fmt.Errorf("workload: stale job %s removed or still terminating; retry creation", job.Name)
	}
	return err
}

// managedJobsFor lists every Job this client manages for one experiment — one for an ungrouped
// job, one per group otherwise. Every read of an experiment's workload goes through this rather
// than a Get on a computed name, because the name of a group's Job is not derivable from the
// experiment id alone, and a lifecycle that could only see the first group would report a job as
// healthy while another group of it was failing.
func (c *JobWorkloadClient) managedJobsFor(ctx context.Context, experimentID string) ([]batchv1.Job, error) {
	list, err := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s", workloadkeys.ManagedBy, workloadkeys.ManagedByValue, workloadkeys.ExperimentID, experimentID),
	})
	if err != nil {
		return nil, fmt.Errorf("workload: list jobs for %s: %w", experimentID, err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

func existingJobMatchesDesired(existing, desired *batchv1.Job, experimentID string) bool {
	return existing != nil && desired != nil && existing.DeletionTimestamp == nil &&
		existing.Annotations[DesiredSpecHashAnnotation] == desired.Annotations[DesiredSpecHashAnnotation] &&
		existing.Labels[workloadkeys.ManagedBy] == workloadkeys.ManagedByValue &&
		existing.Labels[workloadkeys.ExperimentID] == experimentID
}

// ensureHeadlessService creates the headless (ClusterIP: None) Service a distributed job's
// pods need for the rendezvous name (see HYPOTHESISLOOP_MASTER_ADDR in BuildJob) — named after
// the experiment and selecting its pods by experiment-id label alone, so ONE Service covers every
// node of every group and any node can resolve any other.
func (c *JobWorkloadClient) ensureHeadlessService(ctx context.Context, exp *domain.Experiment, job *batchv1.Job) error {
	labels := map[string]string{
		workloadkeys.ManagedBy:    workloadkeys.ManagedByValue,
		workloadkeys.ExperimentID: exp.ID,
		workloadkeys.AgentID:      sanitizeLabel(exp.AgentID),
		workloadkeys.CapacityTier: string(exp.CapacityTier),
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Spec.Template.Spec.Subdomain,
			Namespace: HypothesisLoopNamespace,
			Labels:    labels,
			Annotations: map[string]string{
				DesiredSpecHashAnnotation: job.Annotations[DesiredSpecHashAnnotation],
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  map[string]string{workloadkeys.ExperimentID: exp.ID},
		},
	}
	_, err := c.kube.CoreV1().Services(HypothesisLoopNamespace).Create(ctx, svc, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existing, getErr := c.kube.CoreV1().Services(HypothesisLoopNamespace).Get(ctx, svc.Name, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		if existing.Annotations[DesiredSpecHashAnnotation] == job.Annotations[DesiredSpecHashAnnotation] && existing.DeletionTimestamp == nil &&
			existing.Labels[workloadkeys.ManagedBy] == workloadkeys.ManagedByValue && existing.Labels[workloadkeys.ExperimentID] == exp.ID {
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
	// Deduplicated: a grouped experiment has one Job per group and this reports experiments, not
	// Jobs. A repeated id would make reconcile consider the same experiment several times per pass.
	ids := make([]string, 0, len(jobs.Items))
	seen := make(map[string]bool, len(jobs.Items))
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
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// ListManagedJobsForStatus returns the experiment IDs of every managed Job, including ones that
// are still terminating.
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
	// Deduplicated for the same reason as ListManagedJobs: one grouped experiment is several
	// Jobs and exactly one status.
	ids := make([]string, 0, len(jobs.Items))
	seen := make(map[string]bool, len(jobs.Items))
	for _, j := range jobs.Items {
		id := j.Labels[workloadkeys.ExperimentID]
		if id == "" {
			// One unidentifiable object must not abort the pass: this list drives every create
			// and delete, so failing it wedged reconcile for the whole cluster until a human
			// intervened (important.md #19).
			log.Printf("workload: skipping managed job %q with no experiment identity", j.Name)
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// WorkloadMatchesDesired compares what is actually in the cluster with a fresh compilation of
// desired state. Kubernetes-defaulted fields never enter the comparison since the pre-create
// desired hash is stored as an annotation.
//
// For a grouped experiment every group's Job must match, and the set of Jobs present must be
// exactly the set desired: an extra Job left behind by a spec that used to have three groups is
// as much a drift as a changed resource request, and reconcile has to delete and rebuild.
func (c *JobWorkloadClient) WorkloadMatchesDesired(ctx context.Context, exp *domain.Experiment) (bool, error) {
	placement, err := c.resolvePlacementFor(ctx, exp)
	if err != nil {
		return false, err
	}
	desired, err := c.BuildJobs(exp, placement)
	if err != nil {
		return false, err
	}
	actual, err := c.managedJobsFor(ctx, exp.ID)
	if err != nil {
		return false, err
	}
	byName := make(map[string]*batchv1.Job, len(actual))
	for i := range actual {
		byName[actual[i].Name] = &actual[i]
	}
	if len(byName) != len(desired) {
		return false, nil
	}
	for _, want := range desired {
		got, present := byName[want.Name]
		if !present {
			return false, nil
		}
		if got.Annotations[DesiredSpecHashAnnotation] != want.Annotations[DesiredSpecHashAnnotation] ||
			got.Labels[workloadkeys.ManagedBy] != workloadkeys.ManagedByValue || got.Labels[workloadkeys.ExperimentID] != exp.ID {
			return false, nil
		}
		wantsTemplate := len(want.Spec.Template.Spec.ResourceClaims) > 0
		draMatches, err := c.draTemplateMatches(ctx, want.Name, exp.ID, wantsTemplate, want.Annotations[DesiredSpecHashAnnotation])
		if err != nil {
			return false, fmt.Errorf("workload: compare auxiliary ResourceClaimTemplate: %w", err)
		}
		if !draMatches {
			return false, nil
		}
	}

	// One Service for the whole experiment, hashed against the first group's Job — the same one
	// ensureHeadlessService stamped it with.
	desiredHash := desired[0].Annotations[DesiredSpecHashAnnotation]
	wantsService := desired[0].Spec.Template.Spec.Subdomain != ""
	service, serviceErr := c.kube.CoreV1().Services(HypothesisLoopNamespace).Get(ctx, jobName(exp.ID), metav1.GetOptions{})
	if serviceErr != nil && !errors.IsNotFound(serviceErr) {
		return false, fmt.Errorf("workload: get auxiliary Service: %w", serviceErr)
	}
	if wantsService != (serviceErr == nil) || (serviceErr == nil && (service.Annotations[DesiredSpecHashAnnotation] != desiredHash ||
		service.Labels[workloadkeys.ManagedBy] != workloadkeys.ManagedByValue || service.Labels[workloadkeys.ExperimentID] != exp.ID)) {
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
	schedulable := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		schedulable[n.Name] = true
		if q, ok := n.Status.Allocatable[resourceName]; ok {
			total += extract(q)
		}
	}
	for _, p := range pods {
		if p.Spec.NodeName == "" {
			continue // actual usage starts only once Kubernetes assigns a node
		}
		// Both sides of the subtraction must describe the same set of nodes. The total counts
		// only schedulable ones, so counting requests from pods on every node made cordoning a
		// busy machine subtract its whole workload from capacity that no longer included it —
		// reported availability collapsed, and admission froze against hardware that was fine.
		if !schedulable[p.Spec.NodeName] {
			continue
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

// GetLiveNodeResourceCapacity reports free CPU/memory/ephemeral-storage per schedulable node:
// the node's allocatable minus the requests of every non-terminal pod already assigned to it.
//
// The same allocatable-minus-requested definition the cluster-wide totals use, kept per node
// because a job runs on one node and must fit that node. A cluster-wide total cannot tell a job
// that fits somewhere from one that fits nowhere.
func (c *JobWorkloadClient) GetLiveNodeResourceCapacity(ctx context.Context) (map[string]map[string]int64, error) {
	nodes, pods, err := c.listSchedulableNodesAndPods(ctx)
	if err != nil {
		return nil, err
	}
	dimensions := []struct {
		key      string
		name     corev1.ResourceName
		quantity func(resource.Quantity) int64
	}{
		{domain.NodeResourceCPUMillicores, corev1.ResourceCPU, func(q resource.Quantity) int64 { return q.MilliValue() }},
		{domain.NodeResourceMemoryBytes, corev1.ResourceMemory, func(q resource.Quantity) int64 { return q.Value() }},
		{domain.NodeResourceStorageBytes, corev1.ResourceEphemeralStorage, func(q resource.Quantity) int64 { return q.Value() }},
	}
	out := make(map[string]map[string]int64, len(nodes))
	for _, n := range nodes {
		free := make(map[string]int64, len(dimensions))
		for _, d := range dimensions {
			var allocatable int64
			if q, ok := n.Status.Allocatable[d.name]; ok {
				allocatable = d.quantity(q)
			}
			var requested int64
			for _, p := range pods {
				if p.Spec.NodeName != n.Name {
					continue
				}
				if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
					continue
				}
				for _, ctr := range p.Spec.Containers {
					if q, ok := ctr.Resources.Requests[d.name]; ok {
						requested += d.quantity(q)
					}
				}
			}
			if free[d.key] = allocatable - requested; free[d.key] < 0 {
				free[d.key] = 0
			}
		}
		out[n.Name] = free
	}
	return out, nil
}

// GetNodeTotalCapacity reports each schedulable node's capacity available to PLATFORM-scheduled
// jobs: node.Status.Allocatable minus the requests of every currently-resident pod that is NOT a
// HypothesisLoop-managed job pod (workloadkeys.ManagedBy != workloadkeys.ManagedByValue) —
// DaemonSets, CNI, monitoring, and anything else permanently resident on the node. HypothesisLoop
// never scheduled that capacity away and will never reclaim it, so counting it as available would
// double-count capacity that is permanently gone.
//
// Platform job pods themselves are deliberately NOT subtracted, unlike GetLiveNodeResourceCapacity's
// free-capacity view: this is the stable per-node denominator fair-share math (domain.FairShare)
// needs, and it must report the same number whether or not a platform job happens to be running
// there right now — a platform job coming and going must not move its own node's fair-share base.
func (c *JobWorkloadClient) GetNodeTotalCapacity(ctx context.Context) (map[string]map[string]int64, error) {
	nodes, pods, err := c.listSchedulableNodesAndPods(ctx)
	if err != nil {
		return nil, err
	}
	dimensions := []struct {
		key      string
		name     corev1.ResourceName
		quantity func(resource.Quantity) int64
	}{
		{domain.NodeResourceCPUMillicores, corev1.ResourceCPU, func(q resource.Quantity) int64 { return q.MilliValue() }},
		{domain.NodeResourceMemoryBytes, corev1.ResourceMemory, func(q resource.Quantity) int64 { return q.Value() }},
		{domain.NodeResourceStorageBytes, corev1.ResourceEphemeralStorage, func(q resource.Quantity) int64 { return q.Value() }},
	}
	out := make(map[string]map[string]int64, len(nodes))
	for _, n := range nodes {
		total := make(map[string]int64, len(dimensions))
		for _, d := range dimensions {
			var allocatable int64
			if q, ok := n.Status.Allocatable[d.name]; ok {
				allocatable = d.quantity(q)
			}
			var nonPlatformRequested int64
			for _, p := range pods {
				if p.Spec.NodeName != n.Name {
					continue
				}
				if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
					continue
				}
				if p.Labels[workloadkeys.ManagedBy] == workloadkeys.ManagedByValue {
					continue // a platform job pod's own request is not subtracted — see doc comment
				}
				for _, ctr := range p.Spec.Containers {
					if q, ok := ctr.Resources.Requests[d.name]; ok {
						nonPlatformRequested += d.quantity(q)
					}
				}
			}
			if total[d.key] = allocatable - nonPlatformRequested; total[d.key] < 0 {
				total[d.key] = 0
			}
		}
		out[n.Name] = total
	}
	return out, nil
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
	schedulableNames := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		schedulableNames[n.Name] = true
	}
	draSnapshots, err := c.liveDRACapacitySnapshots(ctx, schedulableNames)
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

// podDeleteGrace is how long this pod may take between SIGTERM and SIGKILL for this termination:
// its own TerminationGracePeriodSeconds, which compileJob widened to hold the checkpoint window
// the job declared, when the control plane granted this termination that window — and the
// ordinary shutdown grace recorded alongside it otherwise.
//
// A pod with no readable shutdown grace is an error rather than a guess. The two numbers are
// only ever different for a job that declared a checkpoint window, and guessing there would mean
// silently handing an infrastructure failure minutes of hardware it has nothing to do with.
func podDeleteGrace(pod *corev1.Pod, grantCheckpointWindow bool) (int64, error) {
	if grantCheckpointWindow {
		if pod.Spec.TerminationGracePeriodSeconds == nil {
			return 0, fmt.Errorf("workload: pod %s declares no termination grace period", pod.Name)
		}
		return *pod.Spec.TerminationGracePeriodSeconds, nil
	}
	ordinary, err := strconv.ParseInt(pod.Annotations[GraceSecondsAnnotation], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("workload: pod %s has no readable shutdown grace: %w", pod.Name, err)
	}
	return ordinary, nil
}

// signalPods deletes every pod of the experiment with an explicit grace period, which is what
// actually delivers SIGTERM and starts the clock — the whole gang at once, before any Job object
// is touched, so no rank is killed while another is still being told.
//
// grantCheckpointWindow chooses the clock: the pod's own TerminationGracePeriodSeconds, which
// compileJob widened to hold the checkpoint window the job declared, or the ordinary shutdown
// grace recorded alongside it. Both are stated explicitly rather than left to the pod's default,
// because the Job deletion that follows hands these pods to Kubernetes' garbage collector, and a
// collector delete may only ever SHORTEN a grace period that is already running — so the window
// has to be in flight, at its full length, before the Job goes.
func (c *JobWorkloadClient) signalPods(ctx context.Context, experimentID string, grantCheckpointWindow bool) error {
	pods, err := c.kube.CoreV1().Pods(HypothesisLoopNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", workloadkeys.ExperimentID, experimentID),
	})
	if err != nil {
		return err
	}
	var errs []error
	for i := range pods.Items {
		pod := &pods.Items[i]
		grace, graceErr := podDeleteGrace(pod, grantCheckpointWindow)
		if graceErr != nil {
			errs = append(errs, graceErr)
			continue
		}
		delErr := c.kube.CoreV1().Pods(HypothesisLoopNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
		})
		if delErr != nil && !errors.IsNotFound(delErr) {
			// One rank failing to be signalled must not leave the rest unsignalled
			// (important.md #19).
			errs = append(errs, delErr)
		}
	}
	return stderrors.Join(errs...)
}

func (c *JobWorkloadClient) DeleteWorkload(ctx context.Context, experimentID string, grantCheckpointWindow bool) error {
	prop := metav1.DeletePropagationBackground
	// Before the Job objects go, so the pods are already counting down their own grace rather
	// than the garbage collector's.
	signalErr := c.signalPods(ctx, experimentID, grantCheckpointWindow)
	// Every Job carrying this experiment's identity, not one computed name: a grouped experiment
	// has one Job per group, and deleting only the one whose name happens to be derivable would
	// leave the rest of the gang running while the control plane believed the workload was gone.
	jobs, listErr := c.managedJobsFor(ctx, experimentID)
	var jobErrs []error
	for _, job := range jobs {
		err := c.kube.BatchV1().Jobs(HypothesisLoopNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{
			PropagationPolicy: &prop,
		})
		if err != nil && !errors.IsNotFound(err) {
			// One group failing to delete must not leave the others running: keep going and
			// report at the end (important.md #19).
			jobErrs = append(jobErrs, err)
		}
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
	return stderrors.Join(append(jobErrs, signalErr, listErr, svcErr, claimErr)...)
}

func (c *JobWorkloadClient) WaitForJobDeletion(ctx context.Context, experimentID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Every group has to be gone, not just the first: recreating the experiment while one
		// group's Job still exists collides on that name and wedges the new generation.
		jobs, err := c.managedJobsFor(ctx, experimentID)
		if err == nil && len(jobs) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("workload: timed out waiting for job %s deletion", jobName(experimentID))
}

func (c *JobWorkloadClient) PollJobPhase(ctx context.Context, experimentID string) (workload.JobPhase, error) {
	status, err := c.PollJobStatus(ctx, experimentID)
	return status.Phase, err
}

// PollJobStatus reports the phase of the experiment's whole workload, a UID that changes
// whenever any part of it is recreated ("" if nothing exists), and the generation it was created
// for. One List instead of a Get per
// group: two polls a few hundred ms apart could each observe "Active>0" across a
// preempt-then-recreate cycle, making an old Job's disappearance and a new Job's appearance look
// like "no change" to a phase-only comparison. Comparing UID alongside phase (see cluster-agent's
// reportChangedStatuses) catches that, since UID always changes on delete+recreate.
//
// A grouped experiment has one Job per group, and this is where they are read as ONE thing. This
// hand-rolled aggregation is what k8s's own Workload/PodGroup API (scheduling.k8s.io, alpha since
// 1.36 — GenericWorkload/WorkloadWithJob feature gates, off by default, not even exposed on our
// k3s v1.36.2) is meant to replace with native gang status. Not adopted yet: alpha, and Job
// controller integration is only "phase 1" upstream. Revisit once it's GA and commonly enabled.
//
// The group Jobs can and will disagree, so the aggregation is stated as a precedence, worst first:
//
//   - nothing present at all -> Gone. A partial set is NOT Gone: some of the gang is still there.
//   - any group Failed -> Failed. A gang is one unit, so one group failing has already ended the
//     experiment, whatever the others are doing. Kubernetes tears down the failed group's own
//     pods; the survivors go when the control plane moves the experiment out of RUNNING and
//     reconcile deletes what is no longer desired — the same one-way path every other termination
//     takes, with no cross-Job kill command invented for this case.
//   - every group Succeeded -> Succeeded. COMPLETED means every pod of every group exited 0, so
//     one group finishing early is not the job finishing.
//   - any group Running -> Running. Some of the job is executing, so it is executing (and
//     billable) even while another group is still pulling its image.
//   - otherwise Pending.
func (c *JobWorkloadClient) PollJobStatus(ctx context.Context, experimentID string) (agentexec.JobStatus, error) {
	jobs, err := c.managedJobsFor(ctx, experimentID)
	if err != nil {
		return agentexec.JobStatus{Phase: workload.JobPhasePending, Attempt: workload.AttemptUnknown}, err
	}
	if len(jobs) == 0 {
		return agentexec.JobStatus{Phase: workload.JobPhaseGone, Attempt: workload.AttemptUnknown}, nil
	}
	// managedJobsFor is one experiment's Jobs, so every group carries the same attempt; the
	// first is the workload's. Unparseable or absent reads as unknown rather than as a number:
	// the control plane fences on it, and a wrong number is worse than no number.
	attempt := workload.AttemptUnknown
	if raw, ok := jobs[0].Labels[workloadkeys.Attempt]; ok {
		if n, err := strconv.Atoi(raw); err == nil {
			attempt = n
		}
	}
	// Sorted by name (managedJobsFor), so the composite is stable across polls and changes as
	// soon as any one group's Job is replaced.
	uids := make([]string, 0, len(jobs))
	for i := range jobs {
		uids = append(uids, string(jobs[i].UID))
	}
	observed := agentexec.JobStatus{UID: strings.Join(uids, ","), Attempt: attempt}

	succeeded, running := 0, 0
	for i := range jobs {
		switch jobPhase(&jobs[i]) {
		case workload.JobPhaseFailed:
			observed.Phase = workload.JobPhaseFailed
			return observed, nil
		case workload.JobPhaseSucceeded:
			succeeded++
		case workload.JobPhaseRunning:
			running++
		}
	}
	switch {
	case succeeded == len(jobs):
		observed.Phase = workload.JobPhaseSucceeded
	case running > 0:
		observed.Phase = workload.JobPhaseRunning
	default:
		observed.Phase = workload.JobPhasePending
	}
	return observed, nil
}

// jobPhase maps one native Job to a platform phase. Never Gone — a Job that was handed to this
// function exists; absence is the caller's question, not this one's.
func jobPhase(job *batchv1.Job) workload.JobPhase {
	// Check the native JobComplete condition before falling back to a raw Succeeded count:
	// for Indexed jobs with a SuccessPolicy, a non-master worker finishing first bumps
	// Status.Succeeded without the job actually being done.
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return workload.JobPhaseSucceeded
		}
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return workload.JobPhaseFailed
		}
	}
	if job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != batchv1.IndexedCompletion {
		if job.Status.Succeeded > 0 {
			return workload.JobPhaseSucceeded
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
		return workload.JobPhaseRunning
	}
	return workload.JobPhasePending
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

// SupportsMultiNodeJobs is true: Kubernetes runs a job's nodes as an Indexed Job (or one per
// group), with a headless Service giving every node a resolvable name. Reported to the control
// plane on every reconcile so admission places distributed work here and not on a single-node
// runtime.
func (c *JobWorkloadClient) SupportsMultiNodeJobs() bool { return true }

// GetClusterID returns the kube-system namespace's UID: created with the cluster, immutable for
// its life, globally unique, and readable with the credentials the agent already holds. The
// community-standard cluster fingerprint (Karpenter, Prometheus "cluster" relabeling, kubecost
// all use it) — chosen over generating and storing our own because the runtime's config file is
// read-only and shared with the control plane, so there is nowhere to persist a generated one.
// See autoscaler.md's "Cluster identity".
func (c *JobWorkloadClient) GetClusterID(ctx context.Context) (string, error) {
	ns, err := c.kube.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("workload: get kube-system namespace: %w", err)
	}
	return string(ns.UID), nil
}

// ReapTerminal has nothing to do here. A finished k8s Job is removed by the reconcile pass that
// finds it undesired; unlike a container engine, k8s keeps no separate terminal record behind it.
func (c *JobWorkloadClient) ReapTerminal(context.Context, map[string]*domain.Experiment) error {
	return nil
}
