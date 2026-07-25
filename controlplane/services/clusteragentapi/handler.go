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
	"sort"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
)

// Store is the desired-state persistence interface the handler needs.
type Store interface {
	ListDesiredWorkloads(ctx context.Context, clusterName string) ([]*domain.Experiment, error)
	GetExperiment(ctx context.Context, id string) (*domain.Experiment, error)
}

// Handler serves the cluster-agent-facing API.
type Handler struct {
	store  Store
	logger *zap.Logger
	// connectedWithin is how recent a heartbeat must be to count as "connected".
	connectedWithin time.Duration
	// metricsDBURL is where live per-cluster accelerator capacity is written.
	metricsDBURL string
}

// NewHandler constructs a Handler. connectedWithin is how recent a cluster's heartbeat must be
// to count as reachable/connected.
func NewHandler(store Store, connectedWithin time.Duration, metricsDBURL string, logger *zap.Logger) *Handler {
	return &Handler{store: store, connectedWithin: connectedWithin, metricsDBURL: metricsDBURL, logger: logger}
}

// RegisterHuma registers the cluster-agent-facing operations on doc. Paths are relative to
// the /internal/clusters mount. Tagged "cluster-agent" — a separate consumer from the
// research agent, so it gets its own OpenAPI doc / explore.
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	apidocs.Register(doc, huma.Operation{
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
			out[i] = clusterInfo{
				ClusterName: name,
				LastSeenAt:  lastSeen,
				Connected:   connected,
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

	apidocs.Register(doc, huma.Operation{
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
			NodeLabelsByNode:           report.NodeLabelsByNode,
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
		resp.Body.Experiments = exps
		return resp, nil
	})

	apidocs.Register(doc, huma.Operation{
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
		for _, rep := range in.Body.Reports {
			if rep.ExperimentID == "" || rep.Phase == "" {
				return nil, huma.Error400BadRequest("every status report requires experiment_id and phase")
			}
			exp, err := h.store.GetExperiment(ctx, rep.ExperimentID)
			if err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
			if exp == nil || exp.ClusterName != clusterName {
				return nil, huma.Error409Conflict("status report does not belong to this cluster: " + rep.ExperimentID)
			}
			statusSamples = append(statusSamples, metricsdb.JobStatusSample{
				ExperimentID: rep.ExperimentID, ClusterName: clusterName, Phase: rep.Phase,
				AdmittedAcceleratorType: rep.AdmittedAcceleratorType, AdmittedNode: rep.AdmittedNode, At: now,
			})
		}
		if err := metricsdb.RecordJobStatuses(ctx, h.metricsDBURL, clusterName, now, statusSamples); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
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
	CPUAvailableCores            float64                      `json:"cpu_available_cores"`
	CPUTotalCores                float64                      `json:"cpu_total_cores"`
	AcceleratorAvailableByFlavor map[string]int64             `json:"accelerator_available_by_type"`
	AcceleratorTotalByFlavor     map[string]int64             `json:"accelerator_total_by_type"`
	AcceleratorAvailableByNode   map[string]map[string]int64  `json:"accelerator_available_by_node"`
	NodeLabelsByNode             map[string]map[string]string `json:"node_labels_by_node"`
	RAMAvailableBytes            int64                        `json:"ram_available_bytes"`
	RAMTotalBytes                int64                        `json:"ram_total_bytes"`
	StorageAvailableBytes        int64                        `json:"storage_available_bytes"`
	StorageTotalBytes            int64                        `json:"storage_total_bytes"`
}

// clusterInfo is one row of GET /internal/clusters.
type clusterInfo struct {
	ClusterName string    `json:"cluster_name"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	Connected   bool      `json:"connected"`
}

// statusReport is one job-status push from a cluster-agent.
type statusReport struct {
	ExperimentID            string `json:"experiment_id"`
	Phase                   string `json:"phase"` // pending | running | succeeded | failed | gone
	AdmittedAcceleratorType string `json:"admitted_accelerator_type,omitempty"`
	AdmittedNode            string `json:"admitted_node,omitempty"`
}
