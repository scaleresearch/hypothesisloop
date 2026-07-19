package scheduler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/openresearch/controlplane/shared/apidocs"
	"github.com/scaleresearch/openresearch/controlplane/shared/domain"
)

// Handler wires the Scheduler Service to HTTP endpoints. Routes are registered
// via RegisterHuma (see below); the transport layer is Huma.
type Handler struct {
	svc *Service
}

// NewHandler creates a Handler for the given Service.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// reasonError preserves the historical {"reason","message"} envelope used by
// experiment submission rejections (admission decisions), distinct from the
// generic {"error":...} envelope.
type reasonError struct {
	status  int
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e *reasonError) Error() string  { return e.Message }
func (e *reasonError) GetStatus() int { return e.status }

// submitRequest is the wire shape for POST /experiments.
type submitRequest struct {
	ID       string                `json:"id"`
	Metadata domain.ExperimentMeta `json:"metadata"`
	Job      domain.JobSpec        `json:"job"`
}

// RegisterHuma registers the public, research-agent-facing scheduler operations
// at their full paths (/experiments...).
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	apidocs.Register(doc, huma.Operation{
		OperationID: "submit-experiment", Method: "POST", Path: "/experiments",
		Summary: "Submit one job", Tags: []string{"experiments"},
		DefaultStatus: 202,
		Description: "Submit {id, metadata, job}. AcceleratorType/count are derived from the job spec. " +
			"Rejections return {reason,message}; a job that fits no node QUEUES FOREVER instead of erroring — " +
			"poll GET /experiments/{id} and check not_admitted_reason.",
	}, func(ctx context.Context, in *struct{ Body submitRequest }) (*struct{ Body *domain.Experiment }, error) {
		req := in.Body
		if req.ID == "" {
			return nil, huma.Error400BadRequest("experiment id is required")
		}
		meta := req.Metadata
		exp := domain.Experiment{
			ID:                     req.ID,
			ParentID:               meta.ParentID,
			AgentID:                meta.AgentID,
			PlatformExperimentID:   meta.PlatformExperimentID,
			ProjectID:              meta.ProjectID,
			CodeRef:                meta.CodeRef,
			ConfigHash:             meta.ConfigHash,
			DataRef:                meta.DataRef,
			Job:                    req.Job,
			HypothesisID:           meta.HypothesisID,
			Hypothesis:             meta.Hypothesis,
			Objective:              meta.Objective,
			Theory:                 meta.Theory,
			AcceleratorType:        req.Job.AcceleratorType,
			AcceleratorCount:       req.Job.TotalAccelerators(),
			EstimatedDurationHours: meta.EstimatedDurationHours,
			CapacityTier:           meta.CapacityTier,
		}
		if err := h.svc.Submit(ctx, &exp); err != nil {
			var admErr *AdmissionError
			if errors.As(err, &admErr) {
				return nil, &reasonError{status: admissionHTTPStatus(admErr.Reason), Reason: admErr.Reason, Message: admErr.Message}
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body *domain.Experiment }{Body: &exp}, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "list-experiments", Method: "GET", Path: "/experiments",
		Summary: "List experiments", Tags: []string{"experiments"},
		Description: "Filter with ?agent=<id> and/or ?status=<STATUS>.",
	}, func(ctx context.Context, in *struct {
		Agent  string `query:"agent"`
		Status string `query:"status"`
	}) (*struct{ Body []*domain.Experiment }, error) {
		exps, err := h.svc.store.ListExperiments(ctx, domain.ExperimentFilter{
			AgentID: in.Agent, Status: domain.ExperimentStatus(in.Status),
		})
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body []*domain.Experiment }{Body: exps}, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "get-experiment", Method: "GET", Path: "/experiments/{id}",
		Summary: "Get one experiment", Tags: []string{"experiments"},
		Description: "status flows SUBMITTED -> QUEUED -> RUNNING -> COMPLETED/FAILED/EVICTED/REJECTED. " +
			"If stuck QUEUED, not_admitted_reason (e.g. capacity_unavailable) explains why.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *domain.Experiment }, error) {
		exp, err := h.svc.store.GetExperiment(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if exp == nil {
			return nil, huma.Error404NotFound("experiment not found")
		}
		return &struct{ Body *domain.Experiment }{Body: exp}, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "admit-experiment", Method: "POST", Path: "/experiments/{id}/admit",
		Summary: "Force-admit a QUEUED experiment onto a named cluster", Tags: []string{"experiments"},
		Description: "Operator endpoint. Requires an explicit valid cluster_name and still respects capacity accounting.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			ClusterName string `json:"cluster_name"`
		}
	}) (*struct{ Body *domain.Experiment }, error) {
		return h.admit(ctx, in.ID, in.Body.ClusterName)
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "cancel-experiment", Method: "POST", Path: "/experiments/{id}/cancel",
		Summary: "Cancel a QUEUED or RUNNING experiment", Tags: []string{"experiments"},
		Description: "Credits are refunded.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := h.svc.CancelExperiment(ctx, in.ID); err != nil {
			if e := admissionStatusError(err); e != nil {
				return nil, e
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "cancelled"
		return out, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "write-experiment-summary", Method: "POST", Path: "/experiments/{id}/summary",
		Summary: "File a summary for a finished job", Tags: []string{"experiments"},
		Description: "Required after every COMPLETED job, before your next submission. Body: {\"summary\": \"...\"}.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Summary string `json:"summary"`
		}
	}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if in.Body.Summary == "" {
			return nil, huma.Error400BadRequest("summary is required")
		}
		if err := h.svc.WriteExperimentSummary(ctx, in.ID, in.Body.Summary); err != nil {
			if e := admissionStatusError(err); e != nil {
				return nil, e
			}
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "ok"
		return out, nil
	})

	apidocs.Register(doc, huma.Operation{
		OperationID: "reprioritize", Method: "POST", Path: "/experiments/reprioritize",
		Summary: "Trigger an immediate re-prioritization pass", Tags: []string{"experiments"},
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Status string `json:"status"`
		}
	}, error) {
		if err := h.svc.RePrioritize(ctx); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &struct {
			Body struct {
				Status string `json:"status"`
			}
		}{}
		out.Body.Status = "ok"
		return out, nil
	})
}

// admissionStatusError maps an AdmissionError to a huma error with the historical
// status codes ({"error":...} envelope, matching the pre-Huma cancel/summary handlers).
func admissionStatusError(err error) huma.StatusError {
	var admErr *AdmissionError
	if !errors.As(err, &admErr) {
		return nil
	}
	switch admErr.Reason {
	case "not_found":
		return huma.Error404NotFound(admErr.Message)
	case "invalid_state":
		return huma.Error409Conflict(admErr.Message)
	default:
		return huma.Error422UnprocessableEntity(admErr.Message)
	}
}

// admit force-admits a QUEUED experiment onto an operator-named cluster, going through the
// same capacity-claiming transition normal admission uses.
func (h *Handler) admit(ctx context.Context, id, clusterName string) (*struct{ Body *domain.Experiment }, error) {
	if clusterName == "" {
		return nil, huma.Error400BadRequest("cluster_name is required")
	}
	valid := false
	for _, c := range h.svc.workload.ClusterNames() {
		if c == clusterName {
			valid = true
			break
		}
	}
	if !valid {
		return nil, huma.Error400BadRequest("cluster_name is not a configured cluster")
	}

	exp, err := h.svc.store.GetExperiment(ctx, id)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if exp == nil {
		return nil, huma.Error404NotFound("experiment not found")
	}
	if exp.Status != domain.StatusQueued {
		return nil, huma.Error409Conflict("only QUEUED experiments can be admitted")
	}

	gAvail, _, err := h.svc.workload.GetFlavorCapacity(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	fp := exp.Footprint()
	avail, ok := gAvail[clusterName]
	if !ok {
		return nil, huma.Error400BadRequest("cluster_name is not a configured cluster")
	}
	pendingByCluster, err := h.svc.store.ListPendingReservationsByCluster(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	subtractFootprint(avail, pendingByCluster[clusterName])
	if !domain.Fits(avail, fp) {
		return nil, huma.Error422UnprocessableEntity("insufficient capacity on requested cluster")
	}

	if err := h.svc.store.MarkSubmitted(ctx, id, clusterName); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	exp.Status = domain.StatusSubmitted
	exp.ClusterName = clusterName
	exp.UpdatedAt = time.Now().UTC()
	if err := h.svc.store.UpsertPendingReservation(ctx, id, clusterName, fp); err != nil {
		_ = h.svc.store.UpdateExperimentStatus(ctx, id, domain.StatusQueued)
		return nil, huma.Error500InternalServerError("reservation failed: " + err.Error())
	}
	if err := h.svc.workload.CreateWorkload(ctx, exp); err != nil {
		_ = h.svc.store.UpdateExperimentStatus(ctx, id, domain.StatusQueued)
		_ = h.svc.store.DeletePendingReservation(ctx, id)
		return nil, huma.Error500InternalServerError("workload creation failed: " + err.Error())
	}
	return &struct{ Body *domain.Experiment }{Body: exp}, nil
}

// admissionHTTPStatus maps an admission reason to the appropriate HTTP status code.
func admissionHTTPStatus(reason string) int {
	switch reason {
	case ReasonInsufficientCredits:
		return http.StatusPaymentRequired
	case ReasonDuplicate:
		return http.StatusConflict
	case ReasonMalformed:
		return http.StatusBadRequest
	case ReasonSummaryRequired:
		return http.StatusForbidden
	case ReasonRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusUnprocessableEntity
	}
}
