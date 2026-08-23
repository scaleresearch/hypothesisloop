// Package clusteragentapi exposes the HTTP surface cluster-agents call — and only
// cluster-agents call. Every request in this package is initiated by an agent running
// inside a target cluster; the control plane never calls out. Endpoints:
//
//	POST /internal/clusters/{name}/reconcile — actual capacity in, desired workloads out
//	POST /internal/clusters/{name}/status        — push job phase observations
//
// There is no command/ack protocol: an experiment's own status (SUBMITTED/ADMITTED/RUNNING)
// is the single source of truth for "should a Job exist" — the agent fetches that view and
// reconciles its local Jobs to match, the same way a kubelet reconciles pods to a desired
// spec. This means desired-state is naturally idempotent and safe to poll from any number of
// concurrent cluster-agent replicas without coordination.
//
// This is a distinct *consumer* surface from the research-agent/dashboard API: it is served
// with its own Huma registration, its own /internal/clusters/openapi.json and its own
// /internal/clusters/explore digest, tagged "cluster-agent", so the Go cluster-agent binary's
// contract is never mixed into the Python research agent's discovery doc.
package clusteragentapi

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/objectstore"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// Store is the desired-state persistence interface the handler needs.
type Store interface {
	ListDesiredWorkloads(ctx context.Context, clusterName string) ([]*domain.Experiment, error)
	// ListRecentlyUndesiredWorkloads returns the experiments that have just stopped being
	// desired — the ones an agent is about to delete — so the reconcile response can name which
	// of them earned a checkpoint window. Not per-cluster: a termination clears the row's
	// cluster_name in the same statement that records it, so the cluster is no longer on the row
	// to filter by (see db.ListRecentlyUndesiredWorkloads).
	ListRecentlyUndesiredWorkloads(ctx context.Context, within time.Duration) ([]*domain.Experiment, error)
	// PlacementByID resolves many experiments' assigned cluster and current attempt at once — a
	// status push is a whole-cluster snapshot, so this is asked once per push rather than once
	// per job.
	PlacementByID(ctx context.Context, ids []string) (map[string]domain.Placement, error)
}

// Handler serves the cluster-agent-facing API.
type Handler struct {
	store  Store
	logger *zap.Logger
	// connectedWithin is how recent a heartbeat must be to count as "connected".
	connectedWithin time.Duration
	// metricsDBURL is where live per-cluster accelerator capacity is written.
	metricsDBURL string
	// dataStore supplies the durable-data addressing and credentials attached to every desired
	// workload on its way out. This is the only surface that hands credentials to a runtime.
	dataStore *objectstore.Client
	// dataSessionDuration is how long a minted session stays valid. A job outliving its session
	// loses write access mid-run, so this is sized against the longest job the ladder allows,
	// not against the reconcile cadence.
	dataSessionDuration time.Duration
	// maxCheckpointGrace is how far back a reconcile pass looks for workloads that have just
	// left the desired set. It is the configured ceiling on a checkpoint window, which is the
	// longest a granted window can possibly still be running: past it, the grant has expired on
	// its own and there is nothing to hold or clear.
	maxCheckpointGrace time.Duration
}

// NewHandler constructs a Handler. connectedWithin is how recent a cluster's heartbeat must be
// to count as reachable/connected.
func NewHandler(store Store, connectedWithin time.Duration, metricsDBURL string, dataStore *objectstore.Client, dataSessionDuration time.Duration, maxCheckpointGrace time.Duration, logger *zap.Logger) *Handler {
	return &Handler{store: store, connectedWithin: connectedWithin, metricsDBURL: metricsDBURL,
		dataStore: dataStore, dataSessionDuration: dataSessionDuration,
		maxCheckpointGrace: maxCheckpointGrace, logger: logger}
}

// attachDataAccess computes each workload's durable-data address from the experiment's own
// identity — platform experiment, agent, job — and mints credentials scoped to it. It is derived
// on the way out and never stored: three columns already say where the bytes belong, and a
// session is worth less than the time it takes to write it down.
//
// The credentials are a fresh STS session per job per pass, restricted by objectstore.SessionPolicy
// to writing its own prefix and reading its platform experiment's. That is what makes "no agent
// can overwrite another's evidence" a property of the store rather than a convention: a job
// physically holds no key that can write anywhere else.
func (h *Handler) attachDataAccess(ctx context.Context, exps []*domain.Experiment) error {
	for _, exp := range exps {
		policy, err := objectstore.SessionPolicy(h.dataStore.Bucket, exp.PlatformExperimentID, exp.AgentID, exp.ID)
		if err != nil {
			return fmt.Errorf("durable-data credentials for %s: %w", exp.ID, err)
		}
		creds, err := h.dataStore.AssumeRole(ctx, policy, h.dataSessionDuration)
		if err != nil {
			return fmt.Errorf("durable-data credentials for %s: %w", exp.ID, err)
		}
		exp.Data = &domain.DataAccess{
			URI:             h.dataStore.URI(objectstore.JobPrefix(exp.PlatformExperimentID, exp.AgentID, exp.ID)),
			Shared:          h.dataStore.URI(objectstore.PlatformExperimentPrefix(exp.PlatformExperimentID)),
			Endpoint:        h.dataStore.Endpoint,
			Region:          h.dataStore.Region,
			AccessKeyID:     creds.AccessKeyID,
			SecretAccessKey: creds.SecretAccessKey,
			SessionToken:    creds.SessionToken,
		}
	}
	return nil
}

// RegisterHuma registers the cluster-agent-facing operations on doc. Paths are relative to
// the /internal/clusters mount. Tagged "cluster-agent" — a separate consumer from the
// research agent, so it gets its own OpenAPI doc / explore.
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "list-clusters", Method: "GET", Path: "/",
		Summary: "List clusters and their connectivity", Tags: []string{"cluster-agent"},
		Description: "Every cluster that has ever polled, and whether its agent is connected right now.",
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Clusters []clusterInfo `json:"clusters"`
		}
	}, error) {
		heartbeats, err := metricsdb.LiveClusterHeartbeats(ctx, h.metricsDBURL, h.connectedWithin)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		available, total, err := metricsdb.LiveClusterAcceleratorAvailableAndTotal(ctx, h.metricsDBURL, h.connectedWithin)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		names := make([]string, 0, len(heartbeats))
		for name := range heartbeats {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]clusterInfo, len(names))
		now := time.Now()
		for i, name := range names {
			connected := heartbeats[name]
			var lastSeen time.Time
			if connected {
				lastSeen = now
			}
			var busySum, totalSum int64
			for flavor, t := range total[name] {
				totalSum += t
				busySum += t - available[name][flavor]
			}
			out[i] = clusterInfo{
				ClusterName:      name,
				LastSeenAt:       lastSeen,
				Connected:        connected,
				AcceleratorBusy:  busySum,
				AcceleratorTotal: totalSum,
			}
		}
		resp := &struct {
			Body struct {
				Clusters []clusterInfo `json:"clusters"`
			}
		}{}
		resp.Body.Clusters = out
		return resp, nil
	})

	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "cluster-reconcile", Method: "POST", Path: "/{name}/reconcile",
		Summary: "Exchange actual capacity for desired workloads", Tags: []string{"cluster-agent"},
		Description: "Atomically records one complete actual-capacity snapshot in metrics storage, then returns the complete PostgreSQL desired workload set for this cluster, plus the ids of workloads whose termination earns them their declared checkpoint window.",
	}, func(ctx context.Context, in *reconcileInput) (*struct {
		Body reconcileBody
	}, error) {
		clusterName := in.Name

		report := in.Body
		if report.AcceleratorAvailableByFlavor == nil || report.AcceleratorTotalByFlavor == nil || report.AcceleratorAvailableByNode == nil {
			return nil, huma.Error400BadRequest("complete accelerator capacity report is required")
		}
		if report.NodeResourcesByNode == nil {
			return nil, huma.Error400BadRequest("per-node resource capacity report is required")
		}
		if report.NodeResourcesTotalByNode == nil {
			return nil, huma.Error400BadRequest("per-node total resource capacity report is required")
		}
		for node, byResource := range report.NodeResourcesByNode {
			if node == "" {
				return nil, huma.Error400BadRequest("per-node resource report has empty node identity")
			}
			for _, key := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
				if _, present := byResource[key]; !present {
					return nil, huma.Error400BadRequest("per-node resource report for " + node + " is missing " + key)
				}
			}
			for resource, available := range byResource {
				if resource == "" || available < 0 {
					return nil, huma.Error400BadRequest("invalid per-node resource capacity value")
				}
			}
			total, present := report.NodeResourcesTotalByNode[node]
			if !present {
				return nil, huma.Error400BadRequest("per-node total resource report for " + node + " is missing")
			}
			for _, key := range []string{domain.NodeResourceCPUMillicores, domain.NodeResourceMemoryBytes, domain.NodeResourceStorageBytes} {
				totalValue, present := total[key]
				if !present || totalValue < 0 {
					return nil, huma.Error400BadRequest("per-node total resource report for " + node + " is missing or invalid " + key)
				}
				if byResource[key] > totalValue {
					return nil, huma.Error400BadRequest("per-node available resource exceeds total for " + node)
				}
			}
		}
		if report.CPUAvailableCores < 0 || report.CPUTotalCores < 0 || report.CPUAvailableCores > report.CPUTotalCores {
			return nil, huma.Error400BadRequest("invalid CPU capacity values")
		}
		for flavor, total := range report.AcceleratorTotalByFlavor {
			if flavor == "" {
				return nil, huma.Error400BadRequest("accelerator capacity contains empty flavor")
			}
			available, present := report.AcceleratorAvailableByFlavor[flavor]
			if !present || total < 0 || available < 0 || available > total {
				return nil, huma.Error400BadRequest("invalid accelerator capacity values")
			}
		}
		if len(report.AcceleratorAvailableByFlavor) != len(report.AcceleratorTotalByFlavor) {
			return nil, huma.Error400BadRequest("accelerator available and total flavor sets must match")
		}
		availableByNode := make(map[string]int64, len(report.AcceleratorAvailableByFlavor))
		for node, flavors := range report.AcceleratorAvailableByNode {
			if node == "" {
				return nil, huma.Error400BadRequest("per-node accelerator report has empty node identity")
			}
			for flavor, available := range flavors {
				if flavor == "" || available < 0 {
					return nil, huma.Error400BadRequest("invalid per-node accelerator capacity value")
				}
				if _, present := report.AcceleratorTotalByFlavor[flavor]; !present {
					return nil, huma.Error400BadRequest("per-node accelerator flavor is absent from cluster totals")
				}
				availableByNode[flavor] += available
			}
		}
		for flavor, byNode := range availableByNode {
			if byNode > report.AcceleratorAvailableByFlavor[flavor] {
				return nil, huma.Error400BadRequest("per-node accelerator capacity exceeds cluster availability")
			}
		}

		if report.RAMAvailableBytes < 0 || report.RAMTotalBytes < 0 || report.RAMAvailableBytes > report.RAMTotalBytes ||
			report.StorageAvailableBytes < 0 || report.StorageTotalBytes < 0 || report.StorageAvailableBytes > report.StorageTotalBytes {
			return nil, huma.Error400BadRequest("invalid memory or storage capacity values")
		}

		if err := metricsdb.RecordClusterCapacitySnapshot(ctx, h.metricsDBURL, metricsdb.ClusterCapacitySnapshot{
			ClusterName: clusterName, At: time.Now().UTC(),
			CPUAvailable: report.CPUAvailableCores, CPUTotal: report.CPUTotalCores,
			AcceleratorAvailable: report.AcceleratorAvailableByFlavor, AcceleratorTotal: report.AcceleratorTotalByFlavor,
			AcceleratorAvailableByNode: report.AcceleratorAvailableByNode,
			NodeResourcesByNode:        report.NodeResourcesByNode,
			NodeResourcesTotalByNode:   report.NodeResourcesTotalByNode,
			NodeLabelsByNode:           report.NodeLabelsByNode,
			MultiNodeCapable:           report.MultiNodeCapable,
			RAMAvailable:               report.RAMAvailableBytes, RAMTotal: report.RAMTotalBytes,
			StorageAvailable: report.StorageAvailableBytes, StorageTotal: report.StorageTotalBytes,
		}); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}

		exps, err := h.store.ListDesiredWorkloads(ctx, clusterName)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		// The other half of the same view: what should NO LONGER exist, and for how much longer
		// it may. A job the platform itself decided to stop -- preempted, cut, out of quota, out
		// of time -- is fine and its work is worth saving, so it is told termination is coming
		// and gets the window it declared before it is killed. A job the environment or its own
		// code ended gets no window; it simply never appears here.
		//
		// Derived from the termination that is already recorded on the experiment, and expiring
		// with maxCheckpointGrace, so there is no terminating state to write, advance or clear.
		undesired, err := h.store.ListRecentlyUndesiredWorkloads(ctx, h.maxCheckpointGrace)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp := &struct {
			Body reconcileBody
		}{}
		// A workload without usable credentials is not returned at all: the pass fails and the
		// cluster-agent retries in a couple of seconds, leaving every running job untouched.
		// Returning it anyway would start a job that cannot save what it produces.
		if err := h.attachDataAccess(ctx, exps); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp.Body.Experiments = exps
		resp.Body.CheckpointWindow = domain.CheckpointWindowGrants(undesired)
		return resp, nil
	})

	apidocs.Register(doc, apidocs.AudienceInternal, huma.Operation{
		OperationID: "cluster-push-status", Method: "POST", Path: "/{name}/status",
		Summary: "Push job-phase observations from a cluster-agent", Tags: []string{"cluster-agent"},
		Description: "Body: {\"reports\": [...]}. The complete snapshot is validated and written atomically; any invalid report rejects the whole request.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Body struct {
			Reports []statusReport `json:"reports"`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		clusterName := in.Name
		now := time.Now().UTC()
		statusSamples := make([]metricsdb.JobStatusSample, 0, len(in.Body.Reports))
		// A runtime reporting a job as running is an observation that it is alive, and it is the
		// one liveness observation every runtime produces — the per-pod cgroup heartbeat exists
		// only where a node-agent is deployed. Recording it here, once, is what gives runtimes
		// without one (bare metal) any observed runtime at all: without it their jobs were billed
		// only in slivers around whatever metrics they happened to report, their preemption
		// rescale computed a stint of ~0 and requeued them at full estimate, and every rule keyed
		// on "never observed" could not be bounded because it would have condemned all of them.
		var observations []metricsdb.Observation
		ids := make([]string, 0, len(in.Body.Reports))
		for _, rep := range in.Body.Reports {
			if rep.ExperimentID == "" || rep.Phase == "" {
				return nil, huma.Error400BadRequest("every status report requires experiment_id and phase")
			}
			ids = append(ids, rep.ExperimentID)
		}
		placementByID, err := h.store.PlacementByID(ctx, ids)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		for _, rep := range in.Body.Reports {
			// A report for an experiment this cluster no longer owns is dropped, not a reason to
			// reject the push. Mid-reschedule that is the ordinary state — the experiment has
			// already been repointed at its new cluster while the old one is still tearing its
			// workload down — and rejecting the whole snapshot blacked out status for every other
			// job on the old cluster until it finished. Dropping it is also the honest report:
			// the experiment genuinely is not this cluster's any more.
			if reason, ok := rejectReport(rep, placementByID[rep.ExperimentID], clusterName); !ok {
				h.logger.Info(reason, zap.String("experiment", rep.ExperimentID),
					zap.String("cluster", clusterName), zap.Int("reported_attempt", reportedAttempt(rep)),
					zap.Int("current_attempt", placementByID[rep.ExperimentID].AttemptCount))
				continue
			}
			statusSamples = append(statusSamples, metricsdb.JobStatusSample{
				ExperimentID: rep.ExperimentID, ClusterName: clusterName, Phase: rep.Phase,
				AdmittedAcceleratorType: rep.AdmittedAcceleratorType, AdmittedNode: rep.AdmittedNode, At: now,
			})
			if rep.Phase == metricsdb.PhaseRunning {
				labels := map[string]string{"cluster_name": clusterName}
				if rep.AdmittedNode != "" {
					labels["node"] = rep.AdmittedNode
				}
				observations = append(observations, metricsdb.Observation{
					ExperimentID: rep.ExperimentID, At: now, ExtraLabels: labels,
				})
			}
			// LogTail is optional per report (nil when the executor has nothing new, or
			// doesn't implement LogTailer) -- only write when the agent actually sent one, so a
			// job with no fresh output this tick doesn't wipe out its last-known tail.
			if rep.LogTail != nil {
				if err := metricsdb.RecordLogTail(ctx, h.metricsDBURL, rep.ExperimentID, clusterName, rep.LogTail, now); err != nil {
					return nil, huma.Error500InternalServerError(err.Error())
				}
			}
			// Reason/Message/RestartCount are optional the same way: only write when the
			// runtime actually has something to say about why this job's container hasn't
			// started or has been restarting, so a healthy tick doesn't overwrite a still-valid
			// prior explanation with silence.
			if rep.Reason != "" || rep.Message != "" || rep.RestartCount != 0 {
				if err := metricsdb.RecordPhaseDetail(ctx, h.metricsDBURL, rep.ExperimentID, clusterName, rep.Reason, rep.Message, rep.RestartCount, now); err != nil {
					return nil, huma.Error500InternalServerError(err.Error())
				}
			}
		}
		if err := metricsdb.RecordJobStatuses(ctx, h.metricsDBURL, clusterName, now, statusSamples); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if len(observations) > 0 {
			if err := metricsdb.RecordObservations(ctx, h.metricsDBURL, observations); err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
		}
		resp := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		resp.Body.Status = "ok"
		return resp, nil
	})
}

// reconcileBody is one reconcile pass's whole answer: what should exist here, and which of the
// things that should no longer exist are entitled to their checkpoint window on the way out.
type reconcileBody struct {
	Experiments []*domain.Experiment `json:"experiments"`
	// CheckpointWindow names the experiments whose termination was the platform's own decision.
	// Deliberately ids and nothing else: the runtime is told which workloads still have a window
	// coming, never the fault class or the rule that produced it. How long each window is was
	// declared on the job's own spec and compiled into the workload when the runtime created it.
	CheckpointWindow []string `json:"checkpoint_window"`
}

type reconcileInput struct {
	Name string `path:"name"`
	Body capacityReport
}

type capacityReport struct {
	CPUAvailableCores            float64                     `json:"cpu_available_cores"`
	CPUTotalCores                float64                     `json:"cpu_total_cores"`
	AcceleratorAvailableByFlavor map[string]int64            `json:"accelerator_available_by_type"`
	AcceleratorTotalByFlavor     map[string]int64            `json:"accelerator_total_by_type"`
	AcceleratorAvailableByNode   map[string]map[string]int64 `json:"accelerator_available_by_node"`
	// NodeResourcesByNode is free CPU/memory/storage per node, keyed by domain.NodeResource*.
	// Required: a job runs on one node and must fit that node in every dimension, and a
	// cluster-wide total cannot answer that — see scheduler.reservePlacement.
	NodeResourcesByNode map[string]map[string]int64 `json:"node_resources_by_node"`
	// NodeResourcesTotalByNode mirrors NodeResourcesByNode but carries each node's installed
	// (allocatable) amount rather than what's currently free — the stable per-node denominator
	// fair-share math needs, since free capacity fluctuates with what's running. Required for the
	// same reason NodeResourcesByNode is: a job's node-local fair share cannot be judged against a
	// number that moves every time something else is scheduled or freed.
	NodeResourcesTotalByNode map[string]map[string]int64  `json:"node_resources_total_by_node"`
	NodeLabelsByNode         map[string]map[string]string `json:"node_labels_by_node"`
	// MultiNodeCapable is whether this cluster's runtime can execute a job spanning more than one
	// node. A capability of the cluster, reported alongside its capacity and read by admission —
	// see agentexec.Executor.SupportsMultiNodeJobs. Absent (false) means single-node only, which
	// is the safe reading: a cluster that has not said it can run distributed work does not get
	// distributed work.
	MultiNodeCapable      bool  `json:"multi_node_capable"`
	RAMAvailableBytes     int64 `json:"ram_available_bytes"`
	RAMTotalBytes         int64 `json:"ram_total_bytes"`
	StorageAvailableBytes int64 `json:"storage_available_bytes"`
	StorageTotalBytes     int64 `json:"storage_total_bytes"`
}

// clusterInfo is one row of GET /internal/clusters.
type clusterInfo struct {
	ClusterName string    `json:"cluster_name"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	Connected   bool      `json:"connected"`
	// AcceleratorBusy/AcceleratorTotal are the cluster's most recently reported occupancy,
	// summed across every accelerator flavor — actual busy-vs-idle chip counts from the last
	// reconcile snapshot, not a budget-consumption ratio. Zero/absent when no live snapshot is
	// within the connectedWithin freshness window (e.g. a disconnected cluster).
	AcceleratorBusy  int64 `json:"accelerator_busy"`
	AcceleratorTotal int64 `json:"accelerator_total"`
}

// statusReport is one job-status push from a cluster-agent. Reason/Message/RestartCount are the
// runtime's explanation for why a job's container hasn't started (or has been restarting) — see
// domain.PhaseDetail. Runtime-agnostic strings: each runtime (k8s, bare-metal) translates its
// own native vocabulary into domain.PhaseReason* before this ever leaves the agent process.
type statusReport struct {
	ExperimentID            string   `json:"experiment_id"`
	Phase                   string   `json:"phase"` // pending | running | succeeded | failed | gone
	AdmittedAcceleratorType string   `json:"admitted_accelerator_type,omitempty"`
	AdmittedNode            string   `json:"admitted_node,omitempty"`
	LogTail                 []string `json:"log_tail,omitempty"`
	Reason                  string   `json:"reason,omitempty"`
	Message                 string   `json:"message,omitempty"`
	RestartCount            int32    `json:"restart_count,omitempty"`
	// Attempt is the generation of the workload this observation came from, as the control plane
	// numbered it (domain.Experiment.AttemptCount, handed to the runtime and carried on the
	// workload it created). A pointer because absent and zero are different answers — see
	// reportedAttempt.
	Attempt *int `json:"attempt,omitempty"`
}

// rejectReport decides whether one status report describes the workload the control plane is
// actually waiting on. ok=false means drop it — never reject the whole push: mid-reschedule a
// stale report is the ordinary state, and failing the snapshot blacks out status for every other
// job on the cluster until it settles.
//
// Two things have to match. Ownership, because the experiment may already have been repointed at
// another cluster while this one is still tearing its workload down. And generation, because
// ownership alone does not cover a retry that lands back on the cluster it just failed on: the
// previous attempt's workload is still terminating and still in this snapshot
// (ListManagedJobsForStatus includes terminating workloads on purpose), so the Failed that caused
// the requeue would be recorded as the observation of the attempt that just started, and fail it
// again immediately.
//
// AttemptUnknown is accepted rather than dropped. It means a cluster-agent that predates the
// field, which can still be trusted for everything ownership already establishes; dropping it
// would black out status for every job on that cluster until it was upgraded — the far larger
// harm, and the reason absence must survive decoding as absence rather than as attempt 0.
func rejectReport(rep statusReport, placement domain.Placement, clusterName string) (string, bool) {
	if placement.ClusterName != clusterName {
		return "dropping status report for an experiment this cluster no longer owns", false
	}
	if attempt := reportedAttempt(rep); attempt != workload.AttemptUnknown && attempt != placement.AttemptCount {
		return "dropping status report from a superseded attempt", false
	}
	return "", true
}

// reportedAttempt is the generation a report belongs to, or workload.AttemptUnknown when the
// cluster-agent named none.
func reportedAttempt(rep statusReport) int {
	if rep.Attempt == nil {
		return workload.AttemptUnknown
	}
	return *rep.Attempt
}
