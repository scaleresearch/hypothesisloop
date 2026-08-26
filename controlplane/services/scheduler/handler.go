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
	// The submit and cancel routes below wake the scheduler loop, and the Service/Loop pair is
	// necessarily wired in two steps (each needs the other). Asserting it here, once, is what
	// lets those routes call Trigger without re-checking on every request.
	if svc.loop == nil {
		panic("scheduler: NewHandler requires a Service with WithLoop already applied")
	}
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

	// Every GET /experiments page is bounded the same way the agent, hypothesis and
	// platform-experiment lists bound theirs. experiments is the fastest-growing table here, so
	// an omitted ?limit must not mean "every row ever submitted" — and the default sits far
	// below the maximum because the caller is an agent reading the response into a bounded
	// context window, not a browser scrolling a table. See db.defaultAgentListLimit.
	const (
		defaultExperimentListLimit = 20
		maxExperimentListLimit     = 200
	)

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-experiments", Method: "GET", Path: "/experiments",
		Summary: "List experiments", Tags: []string{"experiments"},
		Description: "Filter with ?agent, ?platform_experiment_id, ?hypothesis_id, ?project_id, " +
			"?status, ?since (RFC3339; created at or after — the cheap way to catch up on what " +
			"changed since a previous session), ?search (substring match " +
			"against hypothesis/objective/theory), ?needs_summary (finished experiments still missing " +
			"their write-up -- the set that blocks further submission), ?limit (default 20, max 200), ?offset. ?sort selects order: created_at, " +
			"priority_score, or status, optionally prefixed with - for descending (default -created_at " +
			"is NOT the default order — omit ?sort to keep the historical priority-then-recency order). " +
			"Total match count is returned in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		Agent                string `query:"agent" doc:"Only this agent's experiments."`
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only experiments submitted under this platform experiment."`
		HypothesisID         string `query:"hypothesis_id" doc:"Only experiments testing this hypothesis."`
		ProjectID            string `query:"project_id" doc:"Only experiments in this project."`
		Status               string `query:"status" doc:"One of QUEUED, SUBMITTED, ADMITTED, RUNNING, COMPLETED, FAILED, EVICTED, REJECTED. Anything else is rejected rather than ignored, so a typo cannot silently widen the result."`
		Since                string `query:"since" doc:"RFC3339. Only experiments created at or after this instant -- the cheap way to catch up on what changed since a previous session."`
		Search               string `query:"search" doc:"Case-insensitive substring match against hypothesis, objective and theory."`
		NeedsSummary         bool   `query:"needs_summary" doc:"Only finished experiments whose write-up was never filed -- exactly the set the admission summary gate blocks on. Combine with ?agent and ?platform_experiment_id to see what is blocking you, then file each one via POST /experiments/{id}/summary."`
		Limit                int    `query:"limit" doc:"Page size, default 20, maximum 200. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset               int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
		Sort                 string `query:"sort" doc:"created_at, updated_at, priority_score or status, optionally prefixed with '-' for descending. Omit to keep the scheduler's own priority-then-recency order, which is NOT the same as -created_at."`
	}) (*struct {
		Body       []*domain.Experiment
		TotalCount int `header:"X-Total-Count" doc:"Total experiments matching the filter, ignoring limit/offset."`
	}, error) {
		if in.Status != "" && !domain.ValidExperimentStatus(domain.ExperimentStatus(in.Status)) {
			return nil, huma.Error400BadRequest("unknown status " + in.Status)
		}
		if !domain.ValidSortField(in.Sort, domain.ExperimentSortFields) {
			return nil, huma.Error400BadRequest("unknown sort field " + in.Sort)
		}
		if in.Limit < 0 || in.Offset < 0 {
			return nil, huma.Error400BadRequest("limit and offset must not be negative")
		}
		if in.Limit <= 0 {
			in.Limit = defaultExperimentListLimit
		} else if in.Limit > maxExperimentListLimit {
			in.Limit = maxExperimentListLimit
		}
		var since time.Time
		if in.Since != "" {
			parsed, err := time.Parse(time.RFC3339, in.Since)
			if err != nil {
				return nil, huma.Error400BadRequest("since must be RFC3339, got " + in.Since)
			}
			since = parsed
		}
		filter := domain.ExperimentFilter{
			AgentID:              in.Agent,
			PlatformExperimentID: in.PlatformExperimentID,
			HypothesisID:         in.HypothesisID,
			ProjectID:            in.ProjectID,
			Status:               domain.ExperimentStatus(in.Status),
			Since:                since,
			Search:               in.Search,
			NeedsSummary:         in.NeedsSummary,
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
		if err := h.svc.mergePhaseDetails(ctx, exps); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct {
			Body       []*domain.Experiment
			TotalCount int `header:"X-Total-Count" doc:"Total experiments matching the filter, ignoring limit/offset."`
		}{Body: exps, TotalCount: total}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "experiment-stats", Method: "GET", Path: "/experiments/stats",
		Summary: "Count experiments by status, capacity tier and eviction reason", Tags: []string{"experiments"},
		Description: "Whole-set totals for the same ?agent/?platform_experiment_id/?hypothesis_id/" +
			"?project_id/?status/?since/?search filters GET /experiments accepts, counted in " +
			"PostgreSQL. Every list read is bounded to one page, so this is the only way to get a " +
			"total that is not the page's own length -- ask this before listing anything. " +
			"evictions_by_reason is the cheapest diagnosis you have: several jobs evicted " +
			"'silent' or 'never_reported_metrics' means your reporting path is broken, not that " +
			"training is slow, and resubmitting unchanged fails the same way. " +
			"evictions_by_class folds the same tally into whose fault it was: 'workload' is " +
			"yours, 'infrastructure' is the environment's (those cost you no quota and no retry " +
			"attempt -- stop debugging code that was correct), and 'policy' is the platform's " +
			"own decision, such as a stage cut, which is not a failure at all.",
	}, func(ctx context.Context, in *struct {
		Agent                string `query:"agent" doc:"Only this agent's experiments."`
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only experiments submitted under this platform experiment."`
		HypothesisID         string `query:"hypothesis_id" doc:"Only experiments testing this hypothesis."`
		ProjectID            string `query:"project_id" doc:"Only experiments in this project."`
		Status               string `query:"status" doc:"Restrict the counts to one status; see GET /experiments for the valid set."`
		Since                string `query:"since" doc:"RFC3339. Only experiments created at or after this instant."`
		Search               string `query:"search" doc:"Case-insensitive substring match against hypothesis, objective and theory."`
	}) (*struct{ Body *domain.ExperimentStats }, error) {
		if in.Status != "" && !domain.ValidExperimentStatus(domain.ExperimentStatus(in.Status)) {
			return nil, huma.Error400BadRequest("unknown status " + in.Status)
		}
		var since time.Time
		if in.Since != "" {
			parsed, err := time.Parse(time.RFC3339, in.Since)
			if err != nil {
				return nil, huma.Error400BadRequest("since must be RFC3339, got " + in.Since)
			}
			since = parsed
		}
		stats, err := h.svc.store.ExperimentStats(ctx, domain.ExperimentFilter{
			AgentID:              in.Agent,
			PlatformExperimentID: in.PlatformExperimentID,
			HypothesisID:         in.HypothesisID,
			ProjectID:            in.ProjectID,
			Status:               domain.ExperimentStatus(in.Status),
			Since:                since,
			Search:               in.Search,
		})
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &struct{ Body *domain.ExperimentStats }{Body: stats}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-experiment", Method: "GET", Path: "/experiments/{id}",
		Summary: "Get one experiment", Tags: []string{"experiments"},
		Description: "status flows QUEUED -> SUBMITTED -> RUNNING -> COMPLETED/FAILED/EVICTED/REJECTED. " +
			"Carries phase_detail: the runtime's latest reason a job hasn't started or is restarting.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *domain.Experiment }, error) {
		exp, err := h.svc.GetExperiment(ctx, in.ID)
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
		Description: "Recomputes novelty and priority for every QUEUED experiment now, instead of " +
			"waiting for the next scheduled pass. Priority affects admission ORDER only -- it " +
			"never admits a job that does not fit, and never evicts one. A job whose recompute " +
			"fails keeps its previous score rather than blocking the rest of the queue.",
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

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "get-cluster-settings", Method: "GET", Path: "/clusters/{cluster_id}/settings",
		Summary: "Read a cluster's autoscaler-speculation overrides", Tags: []string{"clusters"},
		Description: "Null fields mean the cluster uses the global scheduler defaults.",
	}, func(ctx context.Context, in *struct {
		ClusterID string `path:"cluster_id"`
	}) (*struct {
		Body domain.ClusterSettings
	}, error) {
		cs, err := h.svc.GetClusterSettings(ctx, in.ClusterID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &struct {
			Body domain.ClusterSettings
		}{}
		if cs != nil {
			out.Body = *cs
		} else {
			out.Body = domain.ClusterSettings{ClusterID: in.ClusterID}
		}
		return out, nil
	})

	apidocs.Register(doc, apidocs.AudienceCoordinator, huma.Operation{
		OperationID: "put-cluster-settings", Method: "PUT", Path: "/clusters/{cluster_id}/settings",
		Summary: "Set a cluster's autoscaler-speculation overrides", Tags: []string{"clusters"},
		Description: "scale_up_timeout_seconds overrides the global deadline for this cluster (must be < 1800s — " +
			"PyTorch's default rendezvous timeout). max_speculative_accelerators caps this cluster's outstanding " +
			"speculative footprint. Omit a field (null) to fall back to the global default / no cap.",
	}, func(ctx context.Context, in *struct {
		ClusterID string `path:"cluster_id"`
		Body      struct {
			ScaleUpTimeoutSeconds      *int `json:"scale_up_timeout_seconds,omitempty"`
			MaxSpeculativeAccelerators *int `json:"max_speculative_accelerators,omitempty"`
		}
	}) (*struct {
		Body domain.ClusterSettings
	}, error) {
		cs := &domain.ClusterSettings{
			ClusterID:                  in.ClusterID,
			ScaleUpTimeoutSeconds:      in.Body.ScaleUpTimeoutSeconds,
			MaxSpeculativeAccelerators: in.Body.MaxSpeculativeAccelerators,
		}
		if err := h.svc.PutClusterSettings(ctx, cs); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		out := &struct {
			Body domain.ClusterSettings
		}{}
		out.Body = *cs
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
	// This manual admit path bypasses the ordinary tick's cluster-local "max" resolution (see
	// loop_tick.go's resolveClusterLocalResources) entirely — it names a cluster directly rather
	// than searching for one, so there is no tick-time placement search to hang resolution off of.
	// Refuse rather than silently admit against a footprint that reads every "max" field as
	// zero (see domain.Experiment.Footprint's doc comment): let the ordinary scheduler tick
	// admit a job that still needs "max" resolved instead.
	if jobNeedsMaxResolution(exp.Job) {
		return nil, huma.Error409Conflict("experiment still has an unresolved job.cpu/memory/storage \"max\" sentinel — manual admission does not resolve it; wait for the ordinary scheduler tick to admit this experiment")
	}

	fp := exp.Footprint()
	clusterActive := false
	claimed, err := h.svc.store.ClaimSubmitted(ctx, id, clusterName, nil, func(ctx context.Context, desired []*domain.Experiment) (bool, error) {
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
		nodeResources, err := h.svc.workload.GetNodeResourceCapacity(ctx)
		if err != nil {
			return false, err
		}
		nodeLabels, err := h.svc.workload.GetNodeLabels(ctx)
		if err != nil {
			return false, err
		}
		return desiredPlacementFits(nodeAvail[clusterName], nodeResources[clusterName], nodeLabels[clusterName], desired, exp), nil
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
