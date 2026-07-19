// Package clusteragentapi exposes the HTTP surface cluster-agents call — and only
// cluster-agents call. Every request in this package is initiated by an agent running
// inside a target cluster; the control plane never calls out. Endpoints:
//
//	GET  /internal/clusters/{name}/desired-state — "what should currently be running here?"
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
	"encoding/json"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/openresearch/controlplane/shared/apidocs"
	"github.com/scaleresearch/openresearch/controlplane/shared/db"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
	"github.com/scaleresearch/openresearch/controlplane/shared/metricsdb"
	"github.com/scaleresearch/openresearch/controlplane/shared/obsmetrics"
)

// Store is the persistence interface the handler needs. Satisfied by *db.ClusterQueueStore.
type Store interface {
	ListDesiredWorkloads(ctx context.Context, clusterName string) ([]*domain.Experiment, error)
	UpsertJobReport(ctx context.Context, experimentID, clusterName, phase string, admittedAcceleratorType domain.AcceleratorType, seq int64, jobUID string) (applied bool, uidMismatch bool, err error)
	DeleteJobReportForCluster(ctx context.Context, experimentID, clusterName string) error
	BumpAttemptOnRecreate(ctx context.Context, experimentID string) (attempt int, bumped bool, err error)
	RecordHeartbeat(ctx context.Context, clusterName string, cpuAvailable, cpuTotal float64, hasCPUReport bool) error
	ListHeartbeats(ctx context.Context) ([]db.ClusterHeartbeat, error)
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
		heartbeats, err := h.store.ListHeartbeats(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := make([]clusterInfo, len(heartbeats))
		now := time.Now()
		for i, hb := range heartbeats {
			out[i] = clusterInfo{
				ClusterName: hb.ClusterName,
				LastSeenAt:  hb.LastSeenAt,
				Connected:   now.Sub(hb.LastSeenAt) <= h.connectedWithin,
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
		OperationID: "cluster-desired-state", Method: "GET", Path: "/{name}/desired-state",
		Summary: "Fetch the desired workload set for a cluster", Tags: []string{"cluster-agent"},
		Description: "Returns every experiment that should currently have a Job running in the cluster (full payload). " +
			"Doubles as the cluster-agent heartbeat and live-capacity push channel via optional query params.",
	}, func(ctx context.Context, in *desiredStateInput) (*struct {
		Body struct {
			Experiments []*domain.Experiment `json:"experiments"`
		}
	}, error) {
		clusterName := in.Name

		cpuAvail, availErr := strconv.ParseFloat(in.CPUAvailableCores, 64)
		cpuTotal, totalErr := strconv.ParseFloat(in.CPUTotalCores, 64)
		hasCPUReport := availErr == nil && totalErr == nil && in.CPUAvailableCores != "" && in.CPUTotalCores != ""

		if err := h.store.RecordHeartbeat(ctx, clusterName, cpuAvail, cpuTotal, hasCPUReport); err != nil {
			h.logger.Warn("clusteragentapi: record heartbeat", zap.String("cluster", clusterName), zap.Error(err))
		}

		var acceleratorAvail, acceleratorTotal map[string]int64
		if in.AcceleratorAvailableByFlavor != "" {
			_ = json.Unmarshal([]byte(in.AcceleratorAvailableByFlavor), &acceleratorAvail)
		}
		if in.AcceleratorTotalByFlavor != "" {
			_ = json.Unmarshal([]byte(in.AcceleratorTotalByFlavor), &acceleratorTotal)
		}
		if len(acceleratorAvail) > 0 {
			if err := metricsdb.RecordClusterAcceleratorCapacity(ctx, h.metricsDBURL, clusterName, acceleratorAvail, acceleratorTotal); err != nil {
				h.logger.Warn("clusteragentapi: record accelerator capacity", zap.String("cluster", clusterName), zap.Error(err))
			}
		}

		if ramAvail, availErr := strconv.ParseInt(in.RAMAvailableBytes, 10, 64); availErr == nil {
			if ramTotal, totalErr := strconv.ParseInt(in.RAMTotalBytes, 10, 64); totalErr == nil {
				if err := metricsdb.RecordClusterRAMCapacity(ctx, h.metricsDBURL, clusterName, ramAvail, ramTotal); err != nil {
					h.logger.Warn("clusteragentapi: record RAM capacity", zap.String("cluster", clusterName), zap.Error(err))
				}
			}
		}
		if storageAvail, availErr := strconv.ParseInt(in.StorageAvailableBytes, 10, 64); availErr == nil {
			if storageTotal, totalErr := strconv.ParseInt(in.StorageTotalBytes, 10, 64); totalErr == nil {
				if err := metricsdb.RecordClusterStorageCapacity(ctx, h.metricsDBURL, clusterName, storageAvail, storageTotal); err != nil {
					h.logger.Warn("clusteragentapi: record storage capacity", zap.String("cluster", clusterName), zap.Error(err))
				}
			}
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
		Description: "Body: {\"reports\": [...]}. Returns {status, rejected}; rejected lists reports the write " +
			"did not durably apply (error, stale/duplicate sequence, or not owned by this cluster) — retry those.",
	}, func(ctx context.Context, in *struct {
		Name string `path:"name"`
		Body struct {
			Reports []statusReport `json:"reports"`
		}
	}) (*struct {
		Body struct {
			Status   string   `json:"status"`
			Rejected []string `json:"rejected"`
		}
	}, error) {
		clusterName := in.Name
		rejected := []string{}
		for _, rep := range in.Body.Reports {
			if rep.ExperimentID == "" || rep.Phase == "" {
				continue
			}
			applied, uidMismatch, err := h.store.UpsertJobReport(ctx, rep.ExperimentID, clusterName, rep.Phase, domain.AcceleratorType(rep.AdmittedAcceleratorType), rep.SequenceNumber, rep.JobUID)
			if err != nil {
				h.logger.Warn("clusteragentapi: upsert job report", zap.String("experiment_id", rep.ExperimentID), zap.Error(err))
				rejected = append(rejected, rep.ExperimentID)
				continue
			}
			if !applied {
				rejected = append(rejected, rep.ExperimentID)
				continue
			}
			if h.metricsDBURL != "" && rep.AdmittedNode != "" {
				if err := metricsdb.RecordExperimentNode(ctx, h.metricsDBURL, rep.ExperimentID, rep.AdmittedNode, time.Now().UTC()); err != nil {
					h.logger.Warn("clusteragentapi: record experiment node", zap.String("experiment_id", rep.ExperimentID), zap.Error(err))
				}
			}
			if uidMismatch {
				h.logger.Warn("clusteragentapi: job UID mismatch — report may not correspond to the dispatched Job",
					zap.String("experiment_id", rep.ExperimentID),
					zap.String("cluster", clusterName),
					zap.String("reported_uid", rep.JobUID),
				)
				obsmetrics.JobUIDMismatchTotal.WithLabelValues(clusterName).Inc()
			}
			if rep.Phase == "gone" {
				if err := h.store.DeleteJobReportForCluster(ctx, rep.ExperimentID, clusterName); err != nil {
					h.logger.Warn("clusteragentapi: delete job report", zap.String("experiment_id", rep.ExperimentID), zap.Error(err))
				}
				attempt, bumped, err := h.store.BumpAttemptOnRecreate(ctx, rep.ExperimentID)
				if err != nil {
					h.logger.Warn("clusteragentapi: bump attempt", zap.String("experiment_id", rep.ExperimentID), zap.Error(err))
				} else if bumped {
					h.logger.Warn("clusteragentapi: job disappeared while still desired — recreation attempt bumped",
						zap.String("experiment_id", rep.ExperimentID),
						zap.String("cluster", clusterName),
						zap.Int("attempt", attempt),
					)
				}
			}
		}
		resp := &struct {
			Body struct {
				Status   string   `json:"status"`
				Rejected []string `json:"rejected"`
			}
		}{}
		resp.Body.Status = "ok"
		resp.Body.Rejected = rejected
		return resp, nil
	})
}

// desiredStateInput models the cluster-agent's desired-state poll, including the optional
// capacity/heartbeat query params it piggybacks on the request.
type desiredStateInput struct {
	Name                         string `path:"name"`
	CPUAvailableCores            string `query:"cpu_available_cores"`
	CPUTotalCores                string `query:"cpu_total_cores"`
	AcceleratorAvailableByFlavor string `query:"accelerator_available_by_flavor"`
	AcceleratorTotalByFlavor     string `query:"accelerator_total_by_flavor"`
	RAMAvailableBytes            string `query:"ram_available_bytes"`
	RAMTotalBytes                string `query:"ram_total_bytes"`
	StorageAvailableBytes        string `query:"storage_available_bytes"`
	StorageTotalBytes            string `query:"storage_total_bytes"`
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
	SequenceNumber          int64  `json:"sequence_number"`
	JobUID                  string `json:"job_uid,omitempty"`
}
