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
)

// Store is the desired-state persistence interface the handler needs.
type Store interface {
	ListDesiredWorkloads(ctx context.Context, clusterName string) ([]*domain.Experiment, error)
	// ClusterNameByID resolves many experiments' assigned clusters at once — a status push is a
	// whole-cluster snapshot, so this is asked once per push rather than once per job.
	ClusterNameByID(ctx context.Context, ids []string) (map[string]string, error)
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
}

// NewHandler constructs a Handler. connectedWithin is how recent a cluster's heartbeat must be
// to count as reachable/connected.
func NewHandler(store Store, connectedWithin time.Duration, metricsDBURL string, dataStore *objectstore.Client, dataSessionDuration time.Duration, logger *zap.Logger) *Handler {
	return &Handler{store: store, connectedWithin: connectedWithin, metricsDBURL: metricsDBURL,
		dataStore: dataStore, dataSessionDuration: dataSessionDuration, logger: logger}
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
		Description: "Atomically records one complete actual-capacity snapshot in metrics storage, then returns the complete PostgreSQL desired workload set for this cluster.",
	}, func(ctx context.Context, in *reconcileInput) (*struct {
		Body struct {
			Experiments []*domain.Experiment `json:"experiments"`
		}
	}, error) {
		clusterName := in.Name

		report := in.Body
		if report.AcceleratorAvailableByFlavor == nil || report.AcceleratorTotalByFlavor == nil || report.AcceleratorAvailableByNode == nil {
			return nil, huma.Error400BadRequest("complete accelerator capacity report is required")
		}
		if report.NodeResourcesByNode == nil {
			return nil, huma.Error400BadRequest("per-node resource capacity report is required")
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
		resp := &struct {
			Body struct {
				Experiments []*domain.Experiment `json:"experiments"`
			}
		}{}
		// A workload without usable credentials is not returned at all: the pass fails and the
		// cluster-agent retries in a couple of seconds, leaving every running job untouched.
		// Returning it anyway would start a job that cannot save what it produces.
		if err := h.attachDataAccess(ctx, exps); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		resp.Body.Experiments = exps
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
		clusterByID, err := h.store.ClusterNameByID(ctx, ids)
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
			if clusterByID[rep.ExperimentID] != clusterName {
				h.logger.Info("dropping status report for an experiment this cluster no longer owns",
					zap.String("experiment", rep.ExperimentID), zap.String("cluster", clusterName))
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
	NodeResourcesByNode map[string]map[string]int64  `json:"node_resources_by_node"`
	NodeLabelsByNode    map[string]map[string]string `json:"node_labels_by_node"`
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
}
