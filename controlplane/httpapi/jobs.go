package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/service/scheduler"
)

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

type jobsAPI struct {
	jobs   *scheduler.Service
	access *auth.Access
	logger *zap.Logger
}

// registerJobs registers the public, research-agent-facing scheduler operations at their full
// paths (/experiments...), then the environment and image credential operations.
func registerJobs(doc *Doc, jobs *scheduler.Service, logger *zap.Logger) {
	a := jobsAPI{jobs: jobs, access: doc.Access, logger: logger}
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "submit-experiment", Method: "POST", Path: "/experiments",
		Summary: "Submit one job", Tags: []string{"experiments"},
		DefaultStatus: 202,
		Description: "Submit {metadata, job}. The platform issues the job's id, job-<32 hex>, and answers it as id. AcceleratorType/count are derived from the job spec. " +
			"submission_key makes the submit safe to repeat when its answer was lost: a second submit of the same agent with the same key creates nothing, holds nothing more of the quota and is answered 202 with the job the first created, as it is then, so its status may no longer be QUEUED. The key is the caller's to choose and to change when it means another job. " +
			"metadata.agent_id is whose job it is and whose quota it spends, and metadata.platform_experiment_id where it runs. " +
			"An agent's token says both: it may leave them out, and stating another is refused. Who submitted it is the caller, answered as actor. " +
			"metadata.estimated_duration_hours is what the job holds of its agent's quota until it ends, answered on the job as it was applied. A stated one is never replaced; left out, it is the platform experiment's job_estimated_duration_hours, which the platform measured on its baseline job, and 422 required where it measured none. Nothing is measured before the job of an agent of role baseline: left out there, it is the largest that agent's quota admits now, within the stage's per-job limit and the time left until baseline_due_at, and what the job does not spend is given back when it ends. " +
			"metadata.code_ref is the platform experiment's repository at a commit, repository_url@<40-character sha>, with no credential in it: 422 code_ref_not_in_repository for any other address. " +
			"metadata.branch is the git branch that commit was pushed on, one agent's implementation of the hypothesis, under hl/<platform_experiment_id>/: 422 branch_not_of_experiment for any other. What ran is code_ref, and the branch says which line of work it belongs to. " +
			"job.image is required of every job, 422 required: the name of one of the platform's job images (GET /resource-catalog/capacity, job_images), stored as that image's whole address, or the whole address of another image, registry host first: 422 unknown_job_image for anything else, such as a bare name of an image on a public registry, with the names that are valid. " +
			"422 no_cluster_for_image where the image keeps the job off every cluster that takes the org's jobs, with which image runs where: a cluster with one job image runs the jobs that name it and no other. " +
			"422 experiment_not_started before the platform starts the run, when only the job of an agent of role baseline is accepted: on an open platform experiment the message is its status_reason, the reason and the caller's own next step as GET /platform-experiments/{id} words them. " +
			"job.cpu, job.memory and job.storage, or each group's, are what one node of the job holds, each stated as a quantity (cpu: 2, memory: 8Gi, storage: 4Gi): 422 required, naming the field in problems, where one is left out or is max. A job holds what it states and nothing is sized from the node it lands on. " +
			"422 exceeds_cluster_capacity where the job asks for more than every cluster that runs it has with nothing else on it, with what it asks for and what each cluster has: no wait admits such a job. A cluster that scales up may yet hold it. One that reports nothing now is named and not judged, and where none is judged the job is queued. " +
			"metadata.hypothesis_id and metadata.theory are required of every job but that of an agent of role baseline. The baseline is the objective's own measurement: that job states neither, 422 baseline_tests_no_hypothesis when it names a hypothesis, its branch is hl/<platform_experiment_id>/baseline, and it is answered with objective true and no hypothesis_id: it belongs to the objective, which is no hypothesis to look up. " +
			"Any agent may run its own implementation of any hypothesis of the platform experiment, whoever registered it. " +
			"same_hypothesis on the answer names the jobs of other agents that test the same hypothesis, queued, running or completed within the hour: the job is queued all the same, and whoever steers the run is told it as job.duplicate. " +
			"run_best on the answer names the run's best job on the first ranking metric, its value, commit and branch, where it ran another commit than this job: whether this job's commit contains it is git's to say, which hl job submit asks the submitter's checkout. The job is queued all the same. " +
			"Two fields have a default, answered on the job as applied: metadata.capacity_tier left out is guaranteed, and job.max_retries left out is 0. " +
			"503 object_store_unavailable while the platform's object store does not answer: a submit measures the agent's durable data there and creates the job's data key in it, so nothing was stored and the same submit is repeated later. " +
			"Rejections return {reason,message}; an accepted job remains QUEUED until current capacity fits. Every not_admitted_reason comes with its sentence in not_admitted_message.",
	}, a.submit)
	register(doc, auth.AudienceAgent, huma.Operation{
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
	}, a.list)
	register(doc, auth.AudienceAgent, huma.Operation{
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
	}, a.stats)
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "get-experiment", Method: "GET", Path: "/experiments/{id}",
		Summary: "Get one experiment", Tags: []string{"experiments"},
		Description: "status flows QUEUED -> SUBMITTED -> RUNNING -> COMPLETED/FAILED/EVICTED/REJECTED, and is CANCELLED once somebody cancelled the job, whether or not it ran. " +
			"Carries phase_detail: the runtime's latest reason a job hasn't started or is restarting, " +
			"and metrics: its one value on each declared metric it reported, the last it reported, with metric the one on the first ranking metric. " +
			"A COMPLETED job of a platform experiment with a constraint metric carries rankable, and when that is false not_rankable_reason: " +
			"such a job is in no standing.",
	}, a.get)
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "cancel-experiment", Method: "POST", Path: "/experiments/{id}/cancel",
		Summary: "Cancel a QUEUED or RUNNING experiment", Tags: []string{"experiments"},
		Description: "Credits are refunded. Answers the job, whose status is CANCELLED from then on. 409 invalid_transition for a job that has ended, with the status it ended in: a cancelled one is CANCELLED there as everywhere.",
	}, a.cancel)
	register(doc, auth.AudienceAgent, huma.Operation{
		OperationID: "write-experiment-summary", Method: "POST", Path: "/experiments/{id}/summary",
		Summary: "File a summary for a finished job", Tags: []string{"experiments"},
		Description: "The finding of a finished job, filed by its agent when the result says something: what it showed and why. Body: {\"summary\": \"...\"}. " +
			"Filed once: 409 summary_exists for a job that has one, which is not replaced. " +
			"hypothesis_id and verdict name the job's hypothesis and its status when that verdict is yours to set. " +
			"The job of an agent of role baseline tests no hypothesis: its finding is kept on the objective and the answer names neither. " +
			"The finding is the job's agent's, and who filed it is the caller, answered as actor where the finding is read.",
	}, a.writeSummary)
	registerEnvironments(doc, a)
	registerImageCredentials(doc, a)
}

func (a jobsAPI) submit(ctx context.Context, in *struct{ Body submitRequest }) (*struct{ Body *domain.Experiment }, error) {
	req := in.Body
	meta := req.Metadata
	var err error
	if meta.PlatformExperimentID, meta.AgentID, err = a.access.Spender(ctx, meta.PlatformExperimentID, meta.AgentID); err != nil {
		return nil, err
	}
	caller, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Internal(a.logger, "submit experiment", scheduler.ErrNoCaller)
	}
	exp, err := newJob(req, meta, caller.Actor())
	if err != nil {
		return nil, auth.Internal(a.logger, "submit experiment", err)
	}
	if err := a.jobs.Submit(ctx, exp); err != nil {
		var admErr *scheduler.AdmissionError
		if errors.As(err, &admErr) {
			return nil, admErr.Refusal(scheduler.AdmissionStatus(admErr.Reason))
		}
		return nil, auth.Internal(a.logger, "submit experiment", err)
	}
	if err := a.jobs.NameJobs(ctx, exp); err != nil {
		return nil, auth.Internal(a.logger, "submit experiment", err)
	}
	if err := permitJobs(ctx, a.access, a.logger, []*domain.Experiment{exp}); err != nil {
		return nil, err
	}
	return &struct{ Body *domain.Experiment }{Body: exp}, nil
}

// permitJobs sets on each job what the caller may do to it, by whether it writes the job's
// platform experiment.
func permitJobs(ctx context.Context, access *auth.Access, logger *zap.Logger, jobs []*domain.Experiment) error {
	reader, err := caller(ctx, logger)
	if err != nil {
		return err
	}
	writes := map[string]bool{}
	for _, job := range jobs {
		w, read := writes[job.PlatformExperimentID]
		if !read {
			if w, err = access.Writes(ctx, job.PlatformExperimentID); err != nil {
				return auth.Internal(logger, "read what the caller writes", err, zap.String("platform_experiment_id", job.PlatformExperimentID))
			}
			writes[job.PlatformExperimentID] = w
		}
		job.Actions = scheduler.JobActions(job, reader, w)
	}
	return nil
}

func (a jobsAPI) list(ctx context.Context, in *struct {
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
		return nil, invalid("status", "status is not one of the experiment statuses")
	}
	if !domain.ValidSortField(in.Sort, domain.ExperimentSortFields) {
		return nil, invalid("sort", "sort is not one of created_at, updated_at, priority_score and status, optionally prefixed with '-'")
	}
	if in.Sort == "" {
		in.Sort = "-created_at"
	}
	if in.Limit < 0 || in.Limit > maxExperimentListLimit || in.Offset < 0 {
		return nil, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_page", "limit must be 1 to "+strconv.Itoa(maxExperimentListLimit)+" and offset must not be negative")
	}
	if in.Limit == 0 {
		in.Limit = defaultExperimentListLimit
	}
	var since time.Time
	if in.Since != "" {
		parsed, err := time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return nil, invalid("since", "since must be RFC3339")
		}
		since = parsed
	}
	scope, err := a.access.ListScope(ctx, in.ProjectID)
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
	exps, total, err := a.jobs.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	if err := permitJobs(ctx, a.access, a.logger, exps); err != nil {
		return nil, err
	}
	return &struct {
		Body       []*domain.Experiment
		TotalCount int `header:"X-Total-Count" doc:"Total experiments matching the filter, ignoring limit/offset."`
	}{Body: exps, TotalCount: total}, nil
}

func (a jobsAPI) stats(ctx context.Context, in *struct {
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
		return nil, invalid("group_by", "group_by is agent or hypothesis")
	}
	if in.Status != "" && !domain.ValidExperimentStatus(domain.ExperimentStatus(in.Status)) {
		return nil, invalid("status", "status is not one of the experiment statuses")
	}
	var since time.Time
	if in.Since != "" {
		parsed, err := time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return nil, invalid("since", "since must be RFC3339")
		}
		since = parsed
	}
	scope, err := a.access.ListScope(ctx, in.ProjectID)
	if err != nil {
		return nil, err
	}
	stats, err := a.jobs.Stats(ctx, domain.ExperimentFilter{
		Scope:                scope,
		AgentID:              in.Agent,
		PlatformExperimentID: in.PlatformExperimentID,
		HypothesisID:         in.HypothesisID,
		Status:               domain.ExperimentStatus(in.Status),
		Since:                since,
		Search:               in.Search,
	}, in.GroupBy)
	if err != nil {
		return nil, err
	}
	return &struct{ Body *domain.ExperimentStats }{Body: stats}, nil
}

func (a jobsAPI) get(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body *domain.Experiment }, error) {
	if err := a.access.Experiment(ctx, in.ID); err != nil {
		return nil, err
	}
	exp, err := a.jobs.GetExperiment(ctx, in.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "get experiment", err, zap.String("id", in.ID))
	}
	if exp == nil {
		return nil, huma.Error404NotFound("experiment not found")
	}
	if err := permitJobs(ctx, a.access, a.logger, []*domain.Experiment{exp}); err != nil {
		return nil, err
	}
	return &struct{ Body *domain.Experiment }{Body: exp}, nil
}

func (a jobsAPI) cancel(ctx context.Context, in *struct {
	ID string `path:"id"`
}) (*struct{ Body *domain.Experiment }, error) {
	if err := a.access.OwnExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	if err := a.jobs.CancelExperiment(ctx, in.ID); err != nil {
		if e := scheduler.ChangeRefusal(err); e != nil {
			return nil, e
		}
		return nil, auth.Internal(a.logger, "cancel experiment", err, zap.String("id", in.ID))
	}
	exp, err := a.jobs.GetExperiment(ctx, in.ID)
	if err != nil {
		return nil, auth.Internal(a.logger, "cancel experiment", err, zap.String("id", in.ID))
	}
	if exp == nil {
		return nil, huma.Error404NotFound("experiment not found")
	}
	if err := permitJobs(ctx, a.access, a.logger, []*domain.Experiment{exp}); err != nil {
		return nil, err
	}
	return &struct{ Body *domain.Experiment }{Body: exp}, nil
}

type jobSummaryRequest struct {
	Summary string `json:"summary"`
}

func (a jobsAPI) writeSummary(ctx context.Context, in *struct {
	ID   string `path:"id"`
	Body jobSummaryRequest
}) (*struct{ Body summaryReply }, error) {
	if err := a.access.OwnExperiment(ctx, in.ID); err != nil {
		return nil, err
	}
	if in.Body.Summary == "" {
		return nil, invalid("summary", "summary is required")
	}
	caller, ok := auth.FromContext(ctx)
	if !ok {
		return nil, auth.Internal(a.logger, "write experiment summary", scheduler.ErrNoCaller, zap.String("id", in.ID))
	}
	hypothesis, err := a.jobs.WriteExperimentSummary(ctx, in.ID, caller.Actor(), in.Body.Summary)
	if err != nil {
		if e := scheduler.ChangeRefusal(err); e != nil {
			return nil, e
		}
		return nil, auth.Internal(a.logger, "write experiment summary", err, zap.String("id", in.ID))
	}
	reply := summaryReply{Status: "ok"}
	if hypothesis != nil {
		reply.HypothesisID, reply.Verdict = hypothesis.ID, hypothesis.Status
	}
	return &struct{ Body summaryReply }{Body: reply}, nil
}

func primaryAcceleratorType(job domain.JobSpec) domain.AcceleratorType {
	return job.AcceleratorType
}
