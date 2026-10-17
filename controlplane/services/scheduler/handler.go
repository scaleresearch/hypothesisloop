package scheduler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
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

type summaryReply struct {
	Status       string                  `json:"status"`
	HypothesisID string                  `json:"hypothesis_id,omitempty"`
	Verdict      domain.HypothesisStatus `json:"verdict,omitempty"`
}

// submitRequest is the wire shape for POST /experiments.
type submitRequest struct {
	ID            string         `json:"id,omitempty" required:"false" doc:"Left out: the platform issues the job's id and answers it."`
	SubmissionKey string         `json:"submission_key,omitempty" required:"false" maxLength:"128" pattern:"^[A-Za-z0-9._:-]+$" doc:"Makes the submit safe to repeat: a later submit of the same agent in the same platform experiment with this key creates nothing and is answered with the job this one created, as it is then. hl derives it from the request."`
	Metadata      submitMeta     `json:"metadata"`
	Job           domain.JobSpec `json:"job"`
}

// jobIDPrefix starts the id of every job the platform issues (domain.NewID).
const jobIDPrefix = "job"

// newJob is the job a request states, under the id the platform issues for it.
func newJob(req submitRequest, meta submitMeta, actor domain.Actor) (*domain.Experiment, error) {
	id := req.ID
	if id == "" {
		var err error
		if id, err = domain.NewID(jobIDPrefix); err != nil {
			return nil, err
		}
	}
	return &domain.Experiment{
		ID:                     id,
		SubmissionKey:          req.SubmissionKey,
		ParentID:               meta.ParentID,
		AgentID:                meta.AgentID,
		Actor:                  actor,
		PlatformExperimentID:   meta.PlatformExperimentID,
		CodeRef:                meta.CodeRef,
		Branch:                 meta.Branch,
		ConfigHash:             meta.ConfigHash,
		DataRef:                meta.DataRef,
		Job:                    req.Job,
		HypothesisID:           meta.HypothesisID,
		Hypothesis:             meta.Hypothesis,
		Theory:                 meta.Theory,
		AcceleratorType:        primaryAcceleratorType(req.Job),
		AcceleratorCount:       req.Job.TotalAccelerators(),
		EstimatedDurationHours: meta.EstimatedDurationHours,
		CapacityTier:           meta.CapacityTier,
	}, nil
}

// submitMeta is the metadata of a job as a request states it. The two ids an agent's token
// already says are optional here and resolved by access.Spender.
type submitMeta struct {
	domain.ExperimentMeta
	AgentID              string `json:"agent_id,omitempty" required:"false" doc:"Whose job it is. Absent with an agent's token, it is that agent."`
	PlatformExperimentID string `json:"platform_experiment_id,omitempty" required:"false" doc:"Where it runs. Absent with an agent's token, it is that agent's platform experiment."`
}

// RegisterHuma registers the public, research-agent-facing scheduler operations
// at their full paths (/experiments...).
func RegisterHuma(doc *apidocs.Doc, h *Handler) {
	access := doc.Access

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "submit-experiment", Method: "POST", Path: "/experiments",
		Summary: "Submit one job", Tags: []string{"experiments"},
		DefaultStatus: 202,
		Description: "Submit {metadata, job}. The platform issues the job's id, job-<32 hex>, and answers it as id. AcceleratorType/count are derived from the job spec. " +
			"submission_key makes the submit safe to repeat when its answer was lost: a second submit of the same agent with the same key creates nothing, holds nothing more of the quota and is answered 202 with the job the first created, as it is then, so its status may no longer be QUEUED. The key is the caller's to choose and to change when it means another job. " +
			"metadata.agent_id is whose job it is and whose quota it spends, and metadata.platform_experiment_id where it runs. " +
			"An agent's token says both: it may leave them out, and stating another is refused. Who submitted it is the caller, answered as actor. " +
			"metadata.estimated_duration_hours is what the job holds of its agent's quota until it ends, answered on the job as it was applied. A stated one is never replaced; left out, it is the platform experiment's job_estimated_duration_hours, which the platform measured on its baseline job, and 422 estimate_required where it measured none. Nothing is measured before the job of an agent of role baseline: left out there, it is the largest that agent's quota admits now, within the stage's per-job limit and the time left until baseline_due_at, and what the job does not spend is given back when it ends. " +
			"metadata.code_ref is the platform experiment's repository at a commit, repository_url@<40-character sha>, with no credential in it: 422 code_ref_not_in_repository for any other address. " +
			"metadata.branch is the git branch that commit was pushed on, one agent's implementation of the hypothesis, under hl/<platform_experiment_id>/: 422 branch_not_of_experiment for any other. What ran is code_ref, and the branch says which line of work it belongs to. " +
			"job.image is required of every job, 422 image_required: the name of one of the platform's job images (GET /resource-catalog/capacity, job_images), stored as that image's whole address, or the whole address of another image, registry host first: 422 unknown_job_image for anything else, such as a bare name of an image on a public registry, with the names that are valid. " +
			"422 no_cluster_for_image where the image keeps the job off every cluster that takes the org's jobs, with which image runs where: a cluster with one job image runs the jobs that name it and no other. " +
			"422 experiment_not_started before the platform starts the run, when only the job of an agent of role baseline is accepted: on an open platform experiment the message is its status_reason, the reason and the caller's own next step as GET /platform-experiments/{id} words them. " +
			"job.cpu, job.memory and job.storage, or each group's, are what one node of the job holds, each stated as a quantity (cpu: 2, memory: 8Gi, storage: 4Gi): 422 cpu_required, memory_required or storage_required, naming the field, where one is left out or is max. A job holds what it states and nothing is sized from the node it lands on. " +
			"422 exceeds_cluster_capacity where the job asks for more than every cluster that runs it has with nothing else on it, with what it asks for and what each cluster has: no wait admits such a job. A cluster that scales up may yet hold it. One that reports nothing now is named and not judged, and where none is judged the job is queued. " +
			"metadata.hypothesis_id and metadata.theory are required of every job but that of an agent of role baseline. The baseline is the objective's own measurement: that job states neither, 422 baseline_tests_no_hypothesis when it names a hypothesis, its branch is hl/<platform_experiment_id>/baseline, and it is answered with objective true and no hypothesis_id: it belongs to the objective, which is no hypothesis to look up. " +
			"Any agent may run its own implementation of any hypothesis of the platform experiment, whoever registered it. " +
			"same_hypothesis on the answer names the jobs of other agents that test the same hypothesis, queued, running or completed within the hour: the job is queued all the same, and whoever steers the run is told it as job.duplicate. " +
			"run_best on the answer names the run's best job on the first ranking metric, its value, commit and branch, where it ran another commit than this job: whether this job's commit contains it is git's to say, which hl job submit asks the submitter's checkout. The job is queued all the same. " +
			"Two fields have a default, answered on the job as applied: metadata.capacity_tier left out is guaranteed, and job.max_retries left out is 0. " +
			"503 object_store_unavailable while the platform's object store does not answer: a submit measures the agent's durable data there and creates the job's data key in it, so nothing was stored and the same submit is repeated later. " +
			"Rejections return {reason,message}; an accepted job remains QUEUED until current capacity fits. Every not_admitted_reason comes with its sentence in not_admitted_message.",
	}, func(ctx context.Context, in *struct{ Body submitRequest }) (*struct{ Body *domain.Experiment }, error) {
		req := in.Body
		meta := req.Metadata
		var err error
		if meta.PlatformExperimentID, meta.AgentID, err = access.Spender(ctx, meta.PlatformExperimentID, meta.AgentID); err != nil {
			return nil, err
		}
		caller, ok := auth.FromContext(ctx)
		if !ok {
			return nil, auth.Internal(h.svc.logger, "submit experiment", errNoCaller)
		}
		exp, err := newJob(req, meta, caller.Actor())
		if err != nil {
			return nil, auth.Internal(h.svc.logger, "submit experiment", err)
		}
		if err := h.svc.Submit(ctx, exp); err != nil {
			var admErr *AdmissionError
			if errors.As(err, &admErr) {
				return nil, api.Refuse(admissionHTTPStatus(admErr.Reason), admErr.Reason, admErr.Message)
			}
			return nil, auth.Internal(h.svc.logger, "submit experiment", err)
		}
		if err := h.svc.NameJobs(ctx, exp); err != nil {
			return nil, auth.Internal(h.svc.logger, "submit experiment", err)
		}
		return answerJob(exp), nil
	})

	// Every GET /experiments page is bounded the same way the agent, hypothesis and
	// platform-experiment lists bound theirs. experiments is the fastest-growing table here, so
	// an omitted ?limit must not mean "every row ever submitted" — and the default sits far
	// below the maximum because the caller is an agent reading the response into a bounded
	// context window, not a browser scrolling a table. See db.defaultAgentListLimit.
	const (
		defaultExperimentListLimit = 20
		// Lowered from 200 (2026-09-11): each experiment record duplicates its full job/
		// resolved_job env and repeats hypothesis and theory text, so even a max-size page
		// was ~580KB raw -- too large for an agent to read into context in one call regardless of
		// how it's asked for. 50 caps a raw page at roughly a tenth of that.
		maxExperimentListLimit = 50
	)

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "list-experiments", Method: "GET", Path: "/experiments",
		Summary: "List experiments", Tags: []string{"experiments"},
		Description: "Filter with ?agent_id, ?platform_experiment_id, ?hypothesis_id, ?project_id, " +
			"?status, ?since (RFC3339; created at or after — the cheap way to catch up on what " +
			"changed since a previous session), ?q (substring match " +
			"against hypothesis and theory), ?limit (default 20, max 50, 422 invalid_page over it), ?offset. ?sort selects order: created_at, " +
			"updated_at, priority_score, or status, optionally prefixed with - for descending (default -created_at, newest first; " +
			"-priority_score is the order the scheduler considers queued jobs in). " +
			"Each carries metrics: its one value on each declared metric it reported, the last it reported, and metric, the one on the first ranking metric. " +
			"Each also carries agent_name, what its agent is called now. " +
			"Total match count is returned in the X-Total-Count response header.",
	}, func(ctx context.Context, in *struct {
		Agent                string `query:"agent_id" doc:"Only this agent's experiments."`
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only experiments submitted under this platform experiment."`
		HypothesisID         string `query:"hypothesis_id" doc:"Only experiments testing this hypothesis."`
		ProjectID            string `query:"project_id" doc:"Only experiments in this project."`
		Status               string `query:"status" doc:"One of QUEUED, SUBMITTED, RUNNING, COMPLETED, FAILED, EVICTED, REJECTED, CANCELLED. Anything else is rejected rather than ignored, so a typo cannot silently widen the result."`
		Since                string `query:"since" doc:"RFC3339. Only experiments created at or after this instant -- the cheap way to catch up on what changed since a previous session."`
		Search               string `query:"q" doc:"Case-insensitive substring match against hypothesis and theory."`
		Limit                int    `query:"limit" doc:"Page size, default 20, maximum 50: a larger one is refused with 422 invalid_page. The default is small because the whole response is meant to be read by an agent: ask for more deliberately, using X-Total-Count to decide."`
		Offset               int    `query:"offset" doc:"Rows to skip, for paging through X-Total-Count total matches."`
		Sort                 string `query:"sort" doc:"created_at, updated_at, priority_score or status, optionally prefixed with '-' for descending. Default -created_at, newest first."`
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
		if in.Sort == "" {
			in.Sort = "-created_at"
		}
		if in.Limit < 0 || in.Limit > maxExperimentListLimit || in.Offset < 0 {
			return nil, api.Refuse(http.StatusUnprocessableEntity, "invalid_page", "limit must be 1 to "+strconv.Itoa(maxExperimentListLimit)+" and offset must not be negative")
		}
		if in.Limit == 0 {
			in.Limit = defaultExperimentListLimit
		}
		var since time.Time
		if in.Since != "" {
			parsed, err := time.Parse(time.RFC3339, in.Since)
			if err != nil {
				return nil, huma.Error400BadRequest("since must be RFC3339, got " + in.Since)
			}
			since = parsed
		}
		scope, err := access.ListScope(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		filter := domain.ExperimentFilter{
			Scope:                scope,
			AgentID:              in.Agent,
			PlatformExperimentID: in.PlatformExperimentID,
			HypothesisID:         in.HypothesisID,
			Status:               domain.ExperimentStatus(in.Status),
			Since:                since,
			Search:               in.Search,
			Limit:                in.Limit,
			Offset:               in.Offset,
			Sort:                 in.Sort,
		}
		exps, err := h.svc.store.ListExperiments(ctx, filter)
		if err != nil {
			return nil, auth.Internal(h.svc.logger, "list experiments", err)
		}
		if err := h.svc.NameJobs(ctx, exps...); err != nil {
			return nil, auth.Internal(h.svc.logger, "list experiments", err)
		}
		total, err := h.svc.store.CountExperiments(ctx, filter)
		if err != nil {
			return nil, auth.Internal(h.svc.logger, "list experiments", err)
		}
		if err := h.svc.mergePhaseDetails(ctx, exps); err != nil {
			return nil, auth.Internal(h.svc.logger, "list experiments", err)
		}
		if err := h.svc.mergeRanOn(ctx, exps); err != nil {
			return nil, auth.Internal(h.svc.logger, "list experiments", err)
		}
		if err := h.svc.mergeBuildProgress(ctx, exps); err != nil {
			return nil, auth.Internal(h.svc.logger, "list experiments", err)
		}
		if err := h.svc.mergeMetrics(ctx, exps); err != nil {
			return nil, auth.Internal(h.svc.logger, "list experiments", err)
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
			"own decision, such as a stage cut, which is not a failure at all. " +
			"in_flight_accelerators sums accelerator_count over SUBMITTED and RUNNING jobs on an " +
			"accelerator type, and in_flight_cores does so for jobs on CPU cores. " +
			"?group_by=agent or ?group_by=hypothesis adds groups: the same counts once per " +
			"agent or hypothesis that has a matching experiment, keyed by its id.",
	}, func(ctx context.Context, in *struct {
		Agent                string `query:"agent_id" doc:"Only this agent's experiments."`
		PlatformExperimentID string `query:"platform_experiment_id" doc:"Only experiments submitted under this platform experiment."`
		HypothesisID         string `query:"hypothesis_id" doc:"Only experiments testing this hypothesis."`
		ProjectID            string `query:"project_id" doc:"Only experiments in this project."`
		Status               string `query:"status" doc:"Restrict the counts to one status; see GET /experiments for the valid set."`
		Since                string `query:"since" doc:"RFC3339. Only experiments created at or after this instant."`
		Search               string `query:"q" doc:"Case-insensitive substring match against hypothesis and theory."`
		GroupBy              string `query:"group_by" doc:"agent or hypothesis: also return the counts per agent or per hypothesis, under groups."`
	}) (*struct{ Body *domain.ExperimentStats }, error) {
		if _, ok := domain.ExperimentStatsGroupFields[in.GroupBy]; in.GroupBy != "" && !ok {
			return nil, huma.Error400BadRequest("unknown group_by " + in.GroupBy)
		}
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
		scope, err := access.ListScope(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		stats, err := h.svc.store.ExperimentStats(ctx, domain.ExperimentFilter{
			Scope:                scope,
			AgentID:              in.Agent,
			PlatformExperimentID: in.PlatformExperimentID,
			HypothesisID:         in.HypothesisID,
			Status:               domain.ExperimentStatus(in.Status),
			Since:                since,
			Search:               in.Search,
		}, in.GroupBy)
		if err != nil {
			return nil, auth.Internal(h.svc.logger, "experiment stats", err)
		}
		return &struct{ Body *domain.ExperimentStats }{Body: stats}, nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "get-experiment", Method: "GET", Path: "/experiments/{id}",
		Summary: "Get one experiment", Tags: []string{"experiments"},
		Description: "status flows QUEUED -> SUBMITTED -> RUNNING -> COMPLETED/FAILED/EVICTED/REJECTED, and is CANCELLED once somebody cancelled the job, whether or not it ran. " +
			"Carries phase_detail: the runtime's latest reason a job hasn't started or is restarting, " +
			"and metrics: its one value on each declared metric it reported, the last it reported, with metric the one on the first ranking metric. " +
			"A COMPLETED job of a platform experiment with a constraint metric carries rankable, and when that is false not_rankable_reason: " +
			"such a job is in no standing.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *domain.Experiment }, error) {
		if err := access.Experiment(ctx, in.ID); err != nil {
			return nil, err
		}
		exp, err := h.svc.GetExperiment(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(h.svc.logger, "get experiment", err, zap.String("id", in.ID))
		}
		if exp == nil {
			return nil, huma.Error404NotFound("experiment not found")
		}
		return answerJob(exp), nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "cancel-experiment", Method: "POST", Path: "/experiments/{id}/cancel",
		Summary: "Cancel a QUEUED or RUNNING experiment", Tags: []string{"experiments"},
		Description: "Credits are refunded. Answers the job, whose status is CANCELLED from then on. 409 invalid_transition for a job that has ended, with the status it ended in: a cancelled one is CANCELLED there as everywhere.",
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*struct{ Body *domain.Experiment }, error) {
		if err := access.OwnExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if err := h.svc.CancelExperiment(ctx, in.ID); err != nil {
			if e := admissionStatusError(err); e != nil {
				return nil, e
			}
			return nil, auth.Internal(h.svc.logger, "cancel experiment", err, zap.String("id", in.ID))
		}
		exp, err := h.svc.GetExperiment(ctx, in.ID)
		if err != nil {
			return nil, auth.Internal(h.svc.logger, "cancel experiment", err, zap.String("id", in.ID))
		}
		if exp == nil {
			return nil, huma.Error404NotFound("experiment not found")
		}
		return answerJob(exp), nil
	})

	apidocs.Register(doc, apidocs.AudienceAgent, huma.Operation{
		OperationID: "write-experiment-summary", Method: "POST", Path: "/experiments/{id}/summary",
		Summary: "File a summary for a finished job", Tags: []string{"experiments"},
		Description: "The finding of a finished job, filed by its agent when the result says something: what it showed and why. Body: {\"summary\": \"...\"}. " +
			"Filed once: 409 summary_exists for a job that has one, which is not replaced. " +
			"hypothesis_id and verdict name the job's hypothesis and its status when that verdict is yours to set. " +
			"The job of an agent of role baseline tests no hypothesis: its finding is kept on the objective and the answer names neither. " +
			"The finding is the job's agent's, and who filed it is the caller, answered as actor where the finding is read.",
	}, func(ctx context.Context, in *struct {
		ID   string `path:"id"`
		Body struct {
			Summary string `json:"summary"`
		}
	}) (*struct{ Body summaryReply }, error) {
		if err := access.OwnExperiment(ctx, in.ID); err != nil {
			return nil, err
		}
		if in.Body.Summary == "" {
			return nil, huma.Error400BadRequest("summary is required")
		}
		caller, ok := auth.FromContext(ctx)
		if !ok {
			return nil, auth.Internal(h.svc.logger, "write experiment summary", errNoCaller, zap.String("id", in.ID))
		}
		hypothesis, err := h.svc.WriteExperimentSummary(ctx, in.ID, caller.Actor(), in.Body.Summary)
		if err != nil {
			if e := admissionStatusError(err); e != nil {
				return nil, e
			}
			return nil, auth.Internal(h.svc.logger, "write experiment summary", err, zap.String("id", in.ID))
		}
		reply := summaryReply{Status: "ok"}
		if hypothesis != nil {
			reply.HypothesisID, reply.Verdict = hypothesis.ID, hypothesis.Status
		}
		return &struct{ Body summaryReply }{Body: reply}, nil
	})
	registerEnvironments(doc, h)
	registerImageCredentials(doc, h)
}

func primaryAcceleratorType(job domain.JobSpec) domain.AcceleratorType {
	return job.AcceleratorType
}

// answerJob answers one job. huma adds $schema to an object body by copying its fields into a
// type of its own, so Experiment.MarshalJSON never runs on it and the answer is made here.
func answerJob(exp *domain.Experiment) *struct{ Body *domain.Experiment } {
	return &struct{ Body *domain.Experiment }{Body: exp.Answer()}
}

// ReasonSummaryExists is the reason of the 409 a second summary on one job is answered with.
const ReasonSummaryExists = "summary_exists"

// admissionStatusError is the refusal a cancel or a summary answers an AdmissionError with.
func admissionStatusError(err error) huma.StatusError {
	var admErr *AdmissionError
	if !errors.As(err, &admErr) {
		return nil
	}
	status := http.StatusUnprocessableEntity
	switch admErr.Reason {
	case "not_found":
		status = http.StatusNotFound
	case ReasonInvalidTransition, ReasonSummaryExists:
		status = http.StatusConflict
	}
	return api.Refuse(status, admErr.Reason, admErr.Message)
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
	case ReasonRateLimited:
		return http.StatusTooManyRequests
	case ReasonImageUnresolved:
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnprocessableEntity
	}
}
