package scheduler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "submit-experiment", Method: "POST", Path: "/experiments",
		Summary: "Submit one job", Tags: []string{"experiments"},
		DefaultStatus: 202,
		Description: "Submit {id, metadata, job}. AcceleratorType/count are derived from the job spec. " +
			"Rejections return {reason,message}; an accepted job remains QUEUED until current capacity fits.",
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
			AcceleratorType:        primaryAcceleratorType(req.Job),
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

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-experiments", Method: "GET", Path: "/experiments",
		Summary: "List experiments", Tags: []string{"experiments"},
		Description: "Filter with ?agent, ?platform_experiment_id, ?status, ?search (substring match " +
			"against hypothesis/objective/theory), ?limit, ?offset. ?sort selects order: created_at, " +
			"priority_score, or status, optionally prefixed with - for descending (default -created_at " +
			"is NOT the default order — omit ?sort to keep the historical priority-then-recency order). " +
			"Total match count is returned in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		Agent                string `query:"agent"`
		PlatformExperimentID string `query:"platform_experiment_id"`
		Status               string `query:"status"`
		Search               string `query:"search"`
		Limit                int    `query:"limit"`
		Offset               int    `query:"offset"`
		Sort                 string `query:"sort"`
	}) (*struct {
		Body       []*domain.Experiment
		TotalCount int `header:"X-Total-Count"`
	}, error) {
		filter := domain.ExperimentFilter{
			AgentID:              in.Agent,
			PlatformExperimentID: in.PlatformExperimentID,
			Status:               domain.ExperimentStatus(in.Status),
			Search:               in.Search,
			Limit:                in.Limit,
			Offset:               in.Offset,
			Sort:                 in.Sort,
		}
		exps, err := h.svc.store.ListExperiments(ctx, filter)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		total, err := h.svc.store.CountExperiments(ctx, filter)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct {
			Body       []*domain.Experiment
			TotalCount int `header:"X-Total-Count"`
		}{Body: exps, TotalCount: total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-experiment", Method: "GET", Path: "/experiments/{id}",
		Summary: "Get one experiment", Tags: []string{"experiments"},
		Description: "status flows QUEUED -> SUBMITTED -> RUNNING -> COMPLETED/FAILED/EVICTED/REJECTED.",
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

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
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

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
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

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
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

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
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

func primaryAcceleratorType(job domain.JobSpec) domain.AcceleratorType {
	return job.AcceleratorType
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

	fp := exp.Footprint()
	clusterActive := false
	claimed, err := h.svc.store.ClaimSubmitted(ctx, id, clusterName, func(ctx context.Context, desired []*domain.Experiment) (bool, error) {
		gAvail, _, err := h.svc.workload.GetFlavorCapacity(ctx)
		if err != nil {
			return false, err
		}
		avail, ok := gAvail[clusterName]
		clusterActive = ok
		if !ok || !domain.Fits(avail, fp) {
			return false, nil
		}
		nodeAvail, err := h.svc.workload.GetAcceleratorCapacityByNode(ctx)
		if err != nil {
			return false, err
		}
		nodeLabels, err := h.svc.workload.GetNodeLabels(ctx)
		if err != nil {
			return false, err
		}
		return desiredPlacementFits(nodeAvail[clusterName], nodeLabels[clusterName], desired, exp), nil
	})
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if !claimed {
		if !clusterActive {
			return nil, huma.Error400BadRequest("cluster is not active")
		}
		current, err := h.svc.store.GetExperiment(ctx, id)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if current == nil || current.Status != domain.StatusQueued {
			return nil, huma.Error409Conflict("only QUEUED experiments can be admitted")
		}
		return nil, huma.Error422UnprocessableEntity("insufficient capacity on requested cluster")
	}
	exp.Status = domain.StatusSubmitted
	exp.ClusterName = clusterName
	exp.UpdatedAt = time.Now().UTC()
	if err := h.svc.workload.CreateWorkload(ctx, exp); err != nil {
		if rollbackErr := h.svc.store.MarkQueued(ctx, id, domain.NotAdmittedWorkloadCreation); rollbackErr != nil {
			return nil, huma.Error500InternalServerError("workload creation failed and admission rollback failed: " + rollbackErr.Error())
		}
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
