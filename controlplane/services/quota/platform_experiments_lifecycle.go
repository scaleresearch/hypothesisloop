package quota

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"go.uber.org/zap"
)

var ErrPlatformExperimentNotFound = errors.New("platform experiment not found")

// checkLength holds a platform experiment to a wall clock of at most
// quota.max_platform_experiment_days. Until its end it may rent machines.
func (s *PlatformExperimentsService) checkLength(durationHours float64) error {
	if durationHours <= 0 {
		return refuseUpdate(422, "invalid_length", "duration_hours is required and must be positive: the whole wall clock from starts_at, measuring the baseline included")
	}
	if length := time.Duration(durationHours * float64(time.Hour)); length > s.cfg.MaxRunLength {
		return refuseUpdate(422, "run_too_long", "it would run %.1f days, and a platform experiment runs at most %g days (quota.max_platform_experiment_days): state a shorter duration_hours",
			length.Hours()/24, s.cfg.MaxRunLength.Hours()/24)
	}
	return nil
}

// withSettings returns spec with the platform's settings in the fields a request may leave to
// them: scheduler.default_report_interval_seconds, stages.default and quota.default_quota_tier.
func (s *PlatformExperimentsService) withSettings(spec CreatePlatformExperimentRequest) CreatePlatformExperimentRequest {
	if spec.ReportIntervalSeconds == 0 {
		spec.ReportIntervalSeconds = s.cfg.DefaultReportIntervalSeconds
	}
	if len(spec.Stages) == 0 {
		spec.Stages = s.cfg.DefaultStages
	}
	if spec.DefaultQuotaTier == "" {
		spec.DefaultQuotaTier = string(s.cfg.DefaultQuotaTier)
	}
	return spec
}

// trimTexts returns spec with every text of it without the whitespace around it, which a value
// written as a YAML block ends in. The description alone is more than one line: any other text
// still holding a line break is refused, by its field.
func trimTexts(spec CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	spec.Metrics = slices.Clone(spec.Metrics)
	spec.Description = strings.TrimSpace(spec.Description)
	lines := map[string]*string{"name": &spec.Name, "default_quota_tier": &spec.DefaultQuotaTier}
	for i := range spec.Metrics {
		metric := &spec.Metrics[i]
		lines[fmt.Sprintf("metrics[%d].key", i)] = &metric.Key
		lines[fmt.Sprintf("metrics[%d].direction", i)] = &metric.Direction
		lines[fmt.Sprintf("metrics[%d].role", i)] = (*string)(&metric.Role)
	}
	for _, field := range slices.Sorted(maps.Keys(lines)) {
		*lines[field] = strings.TrimSpace(*lines[field])
		if strings.ContainsAny(*lines[field], "\r\n") {
			return spec, refuseUpdate(422, "invalid_experiment", "%s is one line and holds a line break: state it on a single line", field)
		}
	}
	return spec, nil
}

// validate is the one check of what a platform experiment is, whoever states it: a create, a
// planner's proposal, the person accepting one or a patch of an existing one. It returns spec as it is stored,
// with the platform's settings where spec leaves a field to them.
func (s *PlatformExperimentsService) validate(spec CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	spec, err := trimTexts(s.withSettings(spec))
	if err != nil {
		return spec, err
	}
	if spec.Name == "" || spec.Description == "" {
		return spec, refuseUpdate(422, "invalid_experiment", "name and description are required: the description is the brief every agent works from")
	}
	if err := domain.ValidateMetricDefinitions(spec.Metrics); err != nil {
		return spec, refuseUpdate(422, "invalid_metrics", "metrics: %s", err)
	}
	if err := domain.RequireRankingMetric(spec.Metrics); err != nil {
		return spec, refuseUpdate(422, "invalid_metrics", "metrics: %s", err)
	}
	if spec.ReportIntervalSeconds <= 0 {
		return spec, refuseUpdate(422, "invalid_experiment", "report_interval_seconds must be positive: how often a job really reports")
	}
	if err := domain.ValidateStages(spec.Stages); err != nil {
		return spec, refuseUpdate(422, "invalid_stages", "stages: %s", err)
	}
	if tier := domain.QuotaTier(spec.DefaultQuotaTier); tier != domain.QuotaTierGuaranteed && tier != domain.QuotaTierBurstOnly {
		return spec, refuseUpdate(422, "invalid_experiment", "default_quota_tier must be \"guaranteed\" or \"burst_only\"")
	}
	if spec.MaxConcurrentAccelerators != nil && *spec.MaxConcurrentAccelerators <= 0 {
		return spec, refuseUpdate(422, "invalid_experiment", "max_concurrent_accelerators must be positive")
	}
	if spec.BudgetAcceleratorHours <= 0 {
		return spec, refuseUpdate(422, "invalid_cost", "budget_accelerator_hours is required and must be positive: the cap on everything the run spends")
	}
	if spec.MaxAgents <= 0 {
		return spec, refuseUpdate(422, "invalid_agents", "max_agents is required and must be positive: how many researchers (agents of role competitor) may join")
	}
	return spec, s.checkLength(spec.DurationHours)
}

// requireReachable refuses a platform experiment coming into being where nothing could run it:
// a create in an org whose jobs no cluster takes now, and a proposal or an accept sized for a
// type none of them has. sizedFor is nil for a create, which states no type. One that exists is
// never held to it, since a cluster that reports nothing for a moment must not stop a
// coordinator declaring its baseline or an owner renaming a run. It answers the clusters that
// have the type it was sized for.
func (s *PlatformExperimentsService) requireReachable(ctx context.Context, projectID string, sizedFor *domain.AcceleratorType) ([]domain.Cluster, error) {
	reachable, err := s.reachable(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.requireReachable: %w", err)
	}
	if sizedFor == nil {
		if len(reachable) == 0 {
			return nil, refuseUpdate(422, "accelerator_unreachable", "no cluster this org's jobs can be placed on reports capacity now, so nothing of it could run: register a cluster or turn a shared one on (`hl capacity` lists what is reachable)")
		}
		return nil, nil
	}
	clusters := reachable[string(*sizedFor)]
	if len(clusters) == 0 {
		return nil, refuseUpdate(422, "accelerator_unreachable", "accelerator_type %q is on no cluster this org's jobs can be placed on now: state a type `hl capacity` lists, copied verbatim", *sizedFor)
	}
	return clusters, nil
}

// requireImage refuses a proposal whose image no job on clusters, the ones that have its type,
// could be submitted with, by the two refusals a submit gives a job for its image: one that is
// no image, and one none of them runs. The baseline job is the first to name it, after a person
// accepted.
func requireImage(offered []domain.JobImage, clusters []domain.Cluster, image string) error {
	address, ok := domain.ResolveJobImage(offered, image)
	if !ok {
		return refuseUpdate(422, domain.ReasonUnknownJobImage, "%s", domain.UnknownJobImage(offered, "image", image))
	}
	runEveryJobIn := make([]string, 0, len(clusters))
	for _, cluster := range clusters {
		if cluster.Runs(address) {
			return nil
		}
		runEveryJobIn = append(runEveryJobIn, cluster.Name+" runs every job in "+cluster.JobImage)
	}
	return refuseUpdate(422, domain.NotAdmittedNoClusterForImage, "image %q runs on no cluster that has the accelerator_type: %s. State that image, as `hl capacity` shows it for the cluster", image, strings.Join(runEveryJobIn, "; "))
}

// Create creates a new platform experiment in open status (accepting signups immediately), with
// the agents req carries signed up in the same write.
func (s *PlatformExperimentsService) Create(ctx context.Context, req CreatePlatformExperimentRequest) (*domain.PlatformExperiment, error) {
	now := time.Now().UTC()
	req, err := s.validate(req)
	if err != nil {
		return nil, err
	}
	if _, err := s.requireReachable(ctx, req.ProjectID, nil); err != nil {
		return nil, err
	}
	if req.CoordinatorHosted != (req.coordinator != nil) {
		return nil, fmt.Errorf("platform_experiments.Create: coordinator_hosted is %v and its launcher and model are resolved: %v", req.CoordinatorHosted, req.coordinator != nil)
	}
	id, err := domain.NewID(domain.PlatformExperimentIDPrefix)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	pe := &domain.PlatformExperiment{
		ID:                        id,
		ProjectID:                 req.ProjectID,
		Name:                      req.Name,
		Description:               req.Description,
		BudgetAcceleratorHours:    req.BudgetAcceleratorHours,
		MaxAgents:                 req.MaxAgents,
		Metrics:                   req.Metrics,
		ReportIntervalSeconds:     req.ReportIntervalSeconds,
		StartsAt:                  domain.ClockStart(req.StartsAt, now),
		DurationHours:             req.DurationHours,
		Status:                    domain.PlatformExpOpen,
		Stages:                    req.Stages,
		CurrentStage:              1,
		DefaultQuotaTier:          domain.QuotaTier(req.DefaultQuotaTier),
		MaxConcurrentAccelerators: req.MaxConcurrentAccelerators,
		CreatedAt:                 now,
		UpdatedAt:                 now,
		CoordinatorHosted:         req.CoordinatorHosted,
		Coordinator:               req.coordinator,
		MaxHosted:                 req.maxHosted,
		Repository:                req.repository.Named,
		RepositoryURL:             req.repository.URL,
		RepositoryAccount:         req.repository.CreateOn,
		AgentImage:                req.AgentImage,
		Actor:                     req.createdBy,
	}
	pe.SetClock(s.cfg.BaselineLimit)
	if err := s.store.CreatePlatformExperiment(ctx, pe, req.agents, req.signups); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	s.logger.Info("platform experiment created", zap.String("id", pe.ID), zap.String("name", pe.Name), zap.Int("agents", len(req.signups)))
	pe.SignupCount = len(req.signups)
	s.wakeRepositories(pe)
	return pe, nil
}

// coordinatorWanted says when the platform runs a hosted coordinator's machine (db.coordinatorWantedSQL).
const coordinatorWanted = "The coordinator's machine is asked for once the experiment runs or its status_reason waits for the coordinator (a baseline agent that gave up, several jobs to choose from), and is kept from then until the experiment closes: while the baseline is measured with nothing for it to do, coordinator.instance reads waiting."

// refuseUpdate is a create or a patch the caller can fix.
func refuseUpdate(status int, reason, format string, args ...any) error {
	return api.Refuse(status, reason, fmt.Sprintf(format, args...))
}

// Update changes the fields the patch carries and no others, and the experiment as patched has
// to pass validate. While the experiment is open every
// field of the patch can change. Once it runs, only the name and the description can: admission
// and the stage ladder have already acted on the rest. A closed one, and a draft, which its
// planner's proposal fills, take no patch.
//
// baseline_job_id declares the baseline. An open experiment that has one, or is given one, is
// held as patched to what that job did (checkBaseline), so its metrics and its budget cannot
// leave the baseline behind before it starts, and its description is given what the job is and
// measured (baselineBlock). Stored with a baseline, it is started at once when nothing else
// is missing (StartIfReady), and answered as that left it.
func (s *PlatformExperimentsService) Update(ctx context.Context, id string, patch PlatformExperimentPatch) (*domain.PlatformExperiment, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, refuseUpdate(404, "experiment_not_found", "no platform experiment %s", id)
	}
	if len(patch.carried()) == 0 {
		return nil, refuseUpdate(422, "nothing_to_change", "the body names no field: send the fields that change")
	}
	if pe.Status != domain.PlatformExpOpen && pe.Status != domain.PlatformExpRunning {
		return nil, refuseUpdate(409, "experiment_not_editable", "platform experiment %s is %s: only an open or running one is changed", id, pe.Status)
	}
	if locked := patch.carried("name", "description"); pe.Status == domain.PlatformExpRunning && len(locked) > 0 {
		return nil, refuseUpdate(409, "experiment_running", "only name and description change once a platform experiment runs, and the body also carries: %s. Leave them out", strings.Join(locked, ", "))
	}
	expectedStatus := pe.Status
	patched := *pe
	if patch.Name != nil {
		patched.Name = *patch.Name
	}
	if patch.Description != nil {
		patched.Description = *patch.Description
	}
	if patch.BudgetAcceleratorHours != nil {
		patched.BudgetAcceleratorHours = *patch.BudgetAcceleratorHours
	}
	if patch.MaxAgents != nil {
		researchers, err := s.store.ListSignupsByRole(ctx, id, domain.SignupRoleCompetitor)
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.Update: %w", err)
		}
		if *patch.MaxAgents < len(researchers) {
			return nil, refuseUpdate(409, "max_agents_below_researchers", "max_agents %d is below the %d researchers (agents of role competitor) that joined: remove one first, or state at least %d", *patch.MaxAgents, len(researchers), len(researchers))
		}
		patched.MaxAgents = *patch.MaxAgents
	}
	if patch.Metrics != nil {
		patched.Metrics = *patch.Metrics
	}
	if patch.ReportIntervalSeconds != nil {
		// A create that leaves it out takes the platform's setting. A patch that carries it states it.
		if *patch.ReportIntervalSeconds <= 0 {
			return nil, refuseUpdate(422, "invalid_experiment", "report_interval_seconds must be positive: how often a job really reports. Leave it out to keep %d", pe.ReportIntervalSeconds)
		}
		patched.ReportIntervalSeconds = *patch.ReportIntervalSeconds
	}
	if patch.StartsAt != nil {
		patched.StartsAt = domain.ClockStart(*patch.StartsAt, time.Now().UTC())
	}
	if patch.DurationHours != nil {
		patched.DurationHours = *patch.DurationHours
	}
	if patch.StartsAt != nil || patch.DurationHours != nil {
		patched.SetClock(s.cfg.BaselineLimit)
	}
	if patch.MaxConcurrentAccelerators != nil {
		patched.MaxConcurrentAccelerators = patch.MaxConcurrentAccelerators
	}
	if patch.BaselineJobID != nil {
		patched.BaselineJobID = *patch.BaselineJobID
	}
	// The block that hands the baseline over is the platform's: a patch of the description
	// neither writes it nor loses it.
	authored, _ := splitMeasuredBaseline(patched.Description)
	_, measured := splitMeasuredBaseline(pe.Description)
	checked, err := s.validate(CreatePlatformExperimentRequest{
		Name: patched.Name, Description: authored,
		BudgetAcceleratorHours: patched.BudgetAcceleratorHours, MaxAgents: patched.MaxAgents,
		MaxConcurrentAccelerators: patched.MaxConcurrentAccelerators, Metrics: patched.Metrics,
		ReportIntervalSeconds: patched.ReportIntervalSeconds, StartsAt: patched.StartsAt, DurationHours: patched.DurationHours,
		Stages: patched.Stages, DefaultQuotaTier: string(patched.DefaultQuotaTier),
	})
	if err != nil {
		return nil, err
	}
	patched.Name, patched.Description, patched.Metrics = checked.Name, checked.Description, checked.Metrics
	if pe.Status == domain.PlatformExpOpen && (patch.BaselineJobID != nil || pe.BaselineJobID != "") {
		if measured, err = s.heldToBaseline(ctx, &patched); err != nil {
			return nil, err
		}
	}
	patched.Description = withMeasuredBaseline(patched.Description, measured)
	pe = &patched
	if err := s.store.UpdatePlatformExperiment(ctx, pe, expectedStatus); err != nil {
		return nil, fmt.Errorf("platform_experiments.Update: %w", err)
	}
	if pe.Status != domain.PlatformExpOpen {
		return pe, nil
	}
	if pe.BaselineJobID == "" {
		// The change may be what a job its baseline agent left was refused for: a budget raised
		// to hold a trial apiece, a metric taken out.
		if err := s.DeclareMeasured(ctx, pe.ID); err != nil {
			return nil, err
		}
		return s.stored(ctx, pe.ID)
	}
	// Nothing but the baseline may have been missing: the run starts in the request that
	// declared it, and one that still waits says for what in its status_reason.
	if err := s.StartIfReady(ctx, pe.ID); err != nil {
		return nil, err
	}
	return s.stored(ctx, pe.ID)
}

// heldToBaseline holds pe, as it would be stored, to the job it names as its baseline: the job
// must have done what a baseline does (checkBaseline) and the budget must hold a trial of it for
// each researcher (budgetCoversTrials). It sets on pe the estimate of a job that states none and
// answers the block the brief is given. It is the one check of a declaration, whoever declares.
func (s *PlatformExperimentsService) heldToBaseline(ctx context.Context, pe *domain.PlatformExperiment) (string, error) {
	baseline, err := s.baseline(ctx, pe, pe.BaselineJobID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments: read baseline %s: %w", pe.BaselineJobID, err)
	}
	if err := checkBaseline(pe, baseline); err != nil {
		return "", err
	}
	env, err := s.agentEnvironment(ctx, pe, baseline.Declared)
	if err != nil {
		return "", err
	}
	pe.AgentEnvironmentKey = ""
	if env != nil {
		pe.AgentEnvironmentKey = env.Key
	}
	pe.JobEstimatedDurationHours = baselineHeadroom * baseline.ObservedHours
	if err := budgetCoversTrials(pe, baseline.Job); err != nil {
		return "", err
	}
	return baselineBlock(baseline, pe.Metrics), nil
}

// measuredJob is the job the platform declares as the baseline of the open pe by itself: the one
// COMPLETED job on the objective that reports the ranking metric, among those its baseline agents
// that finished left. With none, or several, it answers the refusal that is the experiment's
// status_reason until someone declares one or another baseline agent finishes: a choice between
// jobs, and what to do about none, need judgement.
func (s *PlatformExperimentsService) measuredJob(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup) (string, error) {
	var joined, measuring, finished, gaveUp []string
	for _, signup := range signups {
		if signup.Role != domain.SignupRoleBaseline {
			continue
		}
		joined = append(joined, signup.Name)
		switch {
		case signup.FinishedAt != nil:
			finished = append(finished, signup.Name)
			gaveUp = append(gaveUp, signup.Name+" finished with the reason \""+signup.FinishedReason+"\"")
		case signup.Instance != nil && signup.Instance.Stopped:
			gaveUp = append(gaveUp, signup.Name+"'s machine stopped with no restart left (its instance says why)")
		default:
			measuring = append(measuring, signup.Name)
		}
	}
	ranked, _ := domain.PrimaryRankingMetric(pe.Metrics)
	var measured []string
	if len(finished) > 0 {
		jobs, err := s.store.FinishedBaselineJobs(ctx, pe.ID)
		if err != nil {
			return "", fmt.Errorf("platform_experiments.measuredJob: %w", err)
		}
		for _, job := range jobs {
			baseline, err := s.baseline(ctx, pe, job.ID)
			if err != nil {
				return "", fmt.Errorf("platform_experiments.measuredJob: read %s: %w", job.ID, err)
			}
			if baseline == nil {
				continue
			}
			if _, reported := baseline.Reported[ranked.Key]; reported {
				measured = append(measured, job.ID)
			}
		}
	}
	switch {
	case len(measured) == 1:
		return measured[0], nil
	case len(measured) > 1:
		return "", api.Refuse(http.StatusConflict, "baseline_undeclared", fmt.Sprintf("the baseline agents of platform experiment %s left %d COMPLETED jobs that report %s (%s), and the platform declares a job by itself only where there is one to declare: its coordinator declares the one the run is ranked against with %s",
			pe.ID, len(measured), ranked.Key, strings.Join(measured, ", "), declareBaseline(pe.ID)))
	case len(measuring) > 0:
		return "", api.Refuse(http.StatusConflict, "baseline_measuring", fmt.Sprintf("baseline agent %s is measuring the baseline of platform experiment %s: when it finishes the platform declares its COMPLETED job and starts the run, and nobody else has anything to do until then",
			strings.Join(measuring, ", "), pe.ID))
	case len(joined) == 0:
		return "", api.Refuse(http.StatusConflict, "baseline_required", fmt.Sprintf("platform experiment %s has no baseline declared and no agent of role baseline to measure one: its coordinator adds one with %s, and the platform declares the job it leaves and starts the run",
			pe.ID, addBaselineAgent(pe.ID)))
	}
	return "", api.Refuse(http.StatusConflict, "baseline_required", fmt.Sprintf("platform experiment %s has no baseline declared, and no baseline agent left a COMPLETED job that reports %s: %s. Its coordinator repairs what the brief can and adds another baseline agent with %s, and what only its owner can repair it writes into the summary",
		pe.ID, ranked.Key, strings.Join(gaveUp, "; "), addBaselineAgent(pe.ID)))
}

// DeclareMeasured declares the baseline of the open platform experiment id when its baseline
// agents left exactly one job to declare (measuredJob), by the declaration a coordinator makes
// (Update), which starts the run when nothing else is missing. The request in which a baseline
// agent finishes calls it, the one that changes what a declaration was refused for, and the
// sweep for what neither could do. A declaration that is refused stores nothing, and the
// experiment's status_reason says the refusal (undeclared) until it is cleared.
func (s *PlatformExperimentsService) DeclareMeasured(ctx context.Context, id string) error {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return err
	}
	if pe.Status != domain.PlatformExpOpen || pe.BaselineJobID != "" {
		return nil
	}
	signups, err := s.store.ListAgentSignups(ctx, id)
	if err != nil {
		return fmt.Errorf("platform_experiments.DeclareMeasured: %w", err)
	}
	jobID, err := s.measuredJob(ctx, pe, signups)
	var refused *api.Refusal
	if errors.As(err, &refused) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := s.Update(ctx, id, PlatformExperimentPatch{BaselineJobID: &jobID}); errors.As(err, &refused) {
		s.logger.Info("the platform did not declare the measured baseline", zap.String("platform_experiment_id", id), zap.String("job_id", jobID), zap.String("reason", refused.Reason))
		return nil
	} else if err != nil {
		return fmt.Errorf("platform_experiments.DeclareMeasured: %w", err)
	}
	s.logger.Info("the platform declared the measured baseline", zap.String("platform_experiment_id", id), zap.String("job_id", jobID))
	return nil
}

// undeclared is why the open pe, which has no baseline, has none: what measuredJob refuses with,
// or for the one job the platform would declare, what the declaration is refused for.
func (s *PlatformExperimentsService) undeclared(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup) (*api.Refusal, error) {
	jobID, err := s.measuredJob(ctx, pe, signups)
	var refused *api.Refusal
	if errors.As(err, &refused) {
		return refused, nil
	}
	if err != nil {
		return nil, err
	}
	declared := *pe
	declared.BaselineJobID = jobID
	if _, err := s.heldToBaseline(ctx, &declared); errors.As(err, &refused) {
		return api.Refuse(http.StatusConflict, refused.Reason, fmt.Sprintf("the platform did not declare job %s, the one job its baseline agents left, as the baseline of platform experiment %s: %s. The platform declares it in the request that clears this",
			jobID, pe.ID, refused.Message)), nil
	} else if err != nil {
		return nil, err
	}
	return api.Refuse(http.StatusConflict, "baseline_declaring", fmt.Sprintf("job %s is the baseline of platform experiment %s and the platform declares it within a minute", jobID, pe.ID)), nil
}

// StartIfReady starts the open platform experiment id when nothing is missing, in the request
// that supplied what was: a declared baseline or a researcher. One that still waits is left open.
// The sweep starts what only time brings: a starts_at that arrives.
func (s *PlatformExperimentsService) StartIfReady(ctx context.Context, id string) error {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return err
	}
	if pe.Status != domain.PlatformExpOpen {
		return nil
	}
	_, err = s.start(ctx, pe, time.Now().UTC())
	var waiting *api.Refusal
	if errors.As(err, &waiting) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("platform_experiments.StartIfReady: %w", err)
	}
	s.logger.Info("started platform experiment", zap.String("platform_experiment_id", id))
	return nil
}

// maxAgentsReached refuses a researcher past max_agents, which counts researchers and nothing else.
func maxAgentsReached(limit int) error {
	return refuseSignup(409, "max_agents_reached", "max_agents is %d and that many researchers (agents of role competitor) have joined. One that finished or was cut still counts, and agents of role baseline, reviewers and the coordinator never do: the owner raises max_agents, or the agent joins with another role", limit)
}

func maxHostedReached(limit int) string {
	return fmt.Sprintf("the platform runs at most %d machines for one platform experiment at once. One is kept for an agent of role baseline, which measures the baseline nothing starts without, and its coordinator and its other hosted agents take the rest. An agent that finished holds none, nor does one whose machine stopped with no restart left", limit)
}

// Signup gives a registered agent its place in a platform experiment: a fixed role, and
// everything it is launched with. An empty QuotaTier defers to the experiment's
// default_quota_tier policy (see domain.ResolveQuotaTier).
func (s *PlatformExperimentsService) Signup(ctx context.Context, platformExpID string, signup domain.AgentSignup) error {
	agentID := signup.AgentID
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return err
	}
	if pe == nil {
		return refuseSignup(404, "experiment_not_found", "no platform experiment %s", platformExpID)
	}
	// A member signing up again has what it asked for, whatever the experiment's status or size.
	already, err := s.store.IsSignedUp(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.Signup: %w", err)
	}
	if already {
		return nil
	}
	if pe.Status != domain.PlatformExpOpen {
		return refuseSignup(409, "signup_closed", "experiment is %s", pe.Status)
	}

	inserted, err := s.store.Signup(ctx, platformExpID, signup)
	if errors.Is(err, db.ErrAgentNameTaken) {
		agent, err := s.store.GetAgent(ctx, agentID)
		if err != nil || agent == nil {
			return fmt.Errorf("platform_experiments.Signup: read agent %s: %v", agentID, err)
		}
		return agentNameTaken(agent.Name)
	}
	if errors.Is(err, db.ErrMaxAgentsReached) {
		return maxAgentsReached(pe.MaxAgents)
	}
	if errors.Is(err, db.ErrMaxHostedReached) {
		return refuseSignup(409, "max_hosted_reached", "%s", maxHostedReached(pe.MaxHosted))
	}
	// Not found either way: an agent of another org is not this experiment's to learn of.
	if errors.Is(err, db.ErrAgentOutsideOrg) {
		return refuseSignup(404, "agent_not_found", "no agent %s is registered", agentID)
	}
	if err != nil {
		return fmt.Errorf("platform_experiments.Signup: %w", err)
	}
	if !inserted {
		// The insert is guarded on the experiment still being open, so this is either a
		// concurrent signup of the same agent or a start that committed since the reads above.
		already, err := s.store.IsSignedUp(ctx, platformExpID, agentID)
		if err != nil {
			return fmt.Errorf("platform_experiments.Signup: %w", err)
		}
		if !already {
			return refuseSignup(409, "signup_closed", "experiment started")
		}
	}
	s.logger.Info("agent signed up", zap.String("platformExpID", platformExpID),
		zap.String("agentID", agentID), zap.String("role", string(signup.Role)))
	return nil
}

// RequireActive refuses a request that starts work or spends for an agent that has no further
// part in the platform experiment: it closed, or the agent was cut. A token that is still alive
// to file summaries does not reopen either.
func (s *PlatformExperimentsService) RequireActive(ctx context.Context, platformExpID, agentID string) error {
	pe, err := s.store.GetPlatformExperiment(ctx, platformExpID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RequireActive: %w", err)
	}
	if pe == nil || pe.Status == domain.PlatformExpClosed {
		return api.Refuse(http.StatusConflict, "experiment_closed", "platform experiment "+platformExpID+" is closed")
	}
	cut, err := s.store.IsAgentCut(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RequireActive: %w", err)
	}
	if cut {
		return api.Refuse(http.StatusConflict, "agent_held", "agent "+agentID+" was cut from platform experiment "+platformExpID)
	}
	return nil
}

// notStartable says why the open platform experiment pe, whose roster is signups, is not started
// at now, as a refusal whose reason is what the experiment answers as its status_reason, or nil
// when it is to be started. With no refusal it returns what the researchers share (RunBudget).
func (s *PlatformExperimentsService) notStartable(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup, now time.Time) (float64, error) {
	// Every researcher starts from the baseline and is ranked against it, so none starts, and
	// no machine is rented for one, before it is measured and declared.
	if pe.BaselineJobID == "" {
		return 0, api.Refuse(http.StatusConflict, "baseline_required", fmt.Sprintf("platform experiment %s has no baseline declared, and the platform starts the run only once it has", pe.ID))
	}
	if pe.StartsAt.After(now) {
		return 0, api.Refuse(http.StatusConflict, "starts_at_ahead", fmt.Sprintf("platform experiment %s has its baseline and starts at its starts_at, %s", pe.ID, pe.StartsAt.UTC().Format(time.RFC3339)))
	}
	if !slices.ContainsFunc(signups, func(signup domain.AgentSignup) bool { return signup.SharesBudget() }) {
		return 0, noResearchers(pe.ID)
	}
	runBudget, err := s.RunBudget(ctx, pe)
	if err != nil {
		return 0, err
	}
	if runBudget <= 0 {
		return 0, api.Refuse(http.StatusConflict, "budget_spent", fmt.Sprintf("measuring the baseline spent the whole budget of %g accelerator hours, so nothing is left for the researchers to share: its owner raises budget_accelerator_hours", pe.BudgetAcceleratorHours))
	}
	return runBudget, nil
}

func noResearchers(id string) error {
	return api.Refuse(http.StatusConflict, "no_researchers", fmt.Sprintf("platform experiment %s has its baseline and no researcher to share its budget: add one with `hl platform-experiments agents add --hosted --platform-experiment %s` (POST /platform-experiments/%s/agents), and the platform starts the run", id, id, id))
}

// Spend sets on pe what it has spent of its budget (ConsumedAcceleratorHours). A draft runs no
// job and has none.
func (s *PlatformExperimentsService) Spend(ctx context.Context, pe *domain.PlatformExperiment) error {
	if pe.Status == domain.PlatformExpDraft {
		return nil
	}
	consumed, err := metricsdb.TotalObservedAccH(ctx, s.usage.URL(), pe.CreatedAt, pe.ID)
	if err != nil {
		return fmt.Errorf("platform_experiments.Spend: %w", err)
	}
	pe.ConsumedAcceleratorHours = &consumed
	return nil
}

// draftReason is where a draft stands with its planner.
func draftReason(pe *domain.PlatformExperiment) *domain.StatusReason {
	switch {
	case pe.DeclinedAt != nil:
		return &domain.StatusReason{Reason: "declined", Message: "its planner proposed no experiment, for declined_reason: revise it with POST /platform-experiments/" + pe.ID + "/revise, saying what the planner lacked, or close it. The platform closes it at proposal_expires_at"}
	case pe.ProposedAt != nil:
		return &domain.StatusReason{Reason: "proposed", Message: "its planner proposed an experiment, in proposal: accept it with POST /platform-experiments/" + pe.ID + "/accept, revise it or close it. The platform closes it at proposal_expires_at"}
	case len(pe.Revisions) > 0:
		return &domain.StatusReason{Reason: "revising", Message: "its planner is answering the last of revisions and has until planning_ends_at"}
	}
	return &domain.StatusReason{Reason: "planning", Message: "its planner is working out how to measure the goal and has until planning_ends_at"}
}

// The commands a status_reason names are whole as a person types them, who coordinates where the
// platform hosts no coordinator and whose token names no platform experiment.
func addBaselineAgent(id string) string {
	return "`hl platform-experiments agents add --hosted --role baseline --platform-experiment " + id + "`"
}

func declareBaseline(id string) string {
	return "`hl platform-experiments update --baseline-job JOB --platform-experiment " + id + "`, which writes the job, its code_ref and what it measured into the description and starts the run"
}

// waiting is why the open pe, whose roster is signups, is not running yet: the reason and the
// sentence its status_reason and its roster answer with.
func (s *PlatformExperimentsService) waiting(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup) (*domain.StatusReason, error) {
	_, err := s.notStartable(ctx, pe, signups, time.Now().UTC())
	var refused *api.Refusal
	switch {
	case errors.As(err, &refused) && pe.BaselineJobID == "":
		// Without a baseline the reason is where the measuring stands and whose move it is.
		if refused, err = s.undeclared(ctx, pe, signups); err != nil {
			return nil, err
		}
		return &domain.StatusReason{Reason: refused.Reason, Message: refused.Message}, nil
	case errors.As(err, &refused):
		return &domain.StatusReason{Reason: refused.Reason, Message: refused.Message}, nil
	case err != nil:
		return nil, err
	}
	return &domain.StatusReason{Reason: "starting", Message: "nothing is missing and the platform starts the run within a minute"}, nil
}

// nextStep is what an agent that reads an open pe does while it waits for reason, a sentence that
// ends in a space, or nothing when all it can do is wait. The reason's own sentence says what
// everyone else does.
func nextStep(reason string, signups []domain.AgentSignup, reader auth.Principal) string {
	if reader.Kind != auth.KindAgent {
		return ""
	}
	own := slices.IndexFunc(signups, func(signup domain.AgentSignup) bool { return signup.AgentID == reader.AgentID })
	switch {
	case own < 0 || signups[own].Role != domain.SignupRoleBaseline:
		return "Every job you submit is refused until the platform starts the run: prepare, then end your turn, and the start arrives as an update. "
	case signups[own].FinishedAt == nil && reason == "baseline_measuring":
		return "Your job is admitted now: measure the baseline with one job (hl job submit), file its summary (hl job summary), then hl platform-experiments agents finish, and the platform declares that job. Your quota is half the budget, for the measurement and any rerun of it (hl quota). "
	}
	return ""
}

// researcherLeft says a researcher of signups may still work: one that neither finished nor was
// cut, and whose machine, where the platform runs it, did not stop with no restart left.
func researcherLeft(signups []domain.AgentSignup) bool {
	return slices.ContainsFunc(signups, func(signup domain.AgentSignup) bool {
		stopped := signup.Hosted && signup.Instance != nil && signup.Instance.Stopped
		return signup.Role == domain.SignupRoleCompetitor && signup.Ended == "" && !stopped
	})
}

// runningReason is where a running pe, whose roster is signups, stands with its clock and with
// the researchers that work in it.
func runningReason(pe *domain.PlatformExperiment, signups []domain.AgentSignup) *domain.StatusReason {
	switch {
	case pe.AwaitsAgents():
		return &domain.StatusReason{Reason: "agents_starting", Message: "the run has started and its researchers' machines are starting: the first one running sets started_at. The platform closes it at agents_due_at, with close_reason " + string(domain.CloseAgentsNotStarted) + ", if none is running by then, and at ends_at in any case"}
	case !researcherLeft(signups):
		return &domain.StatusReason{Reason: "researchers_done", Message: fmt.Sprintf("every researcher of platform experiment %s has finished, was cut or lost its machine with no restart left, so nobody is left to submit a job and the run only waits for its ends_at, when the platform closes it: its coordinator writes the summary with `hl platform-experiments summary --file FILE --platform-experiment %s` and closes it with `hl platform-experiments close --platform-experiment %s`",
			pe.ID, pe.ID, pe.ID)}
	}
	return &domain.StatusReason{Reason: "running", Message: "its researchers are at work and the platform closes it at ends_at"}
}

// closedReason is why a closed pe closed: its close_reason and what that means.
func closedReason(pe *domain.PlatformExperiment) *domain.StatusReason {
	how := map[domain.CloseReason]string{
		domain.CloseEndTimeReached:       "the run reached its ends_at and the platform closed it",
		domain.CloseByRequest:            "closed_by asked for it to be closed",
		domain.ClosePlanningLimitReached: "its planner answered nothing by planning_ends_at and the platform closed the draft",
		domain.CloseProposalExpired:      "no person accepted or revised its planner's answer by proposal_expires_at and the platform closed the draft",
		domain.ClosePlannerStopped:       "its planner's machine ended before it answered and the platform had restarted it as often as it restarts one, so it closed the draft: the planner's instance in the roster says how the last machine ended (end_reason, exit_log). Nothing was spent on a run, and the same goal makes a new draft",
		domain.CloseBaselineLimitReached: "it was not running by baseline_due_at and the platform closed it: no baseline was declared, or the start still waited for something",
		domain.CloseAgentsNotStarted:     "it started and no researcher's machine was running by agents_due_at, so the platform closed it. No researcher took a turn: read each agent's instance for why its machine did not come up, and create it again",
		domain.CloseRepositoryNotCreated: "the platform could not create its repository, so none of its machines started and nothing was spent. Create it again, or name a repository. GitHub answered: " + pe.RepositoryError,
	}
	if sentence, known := how[pe.CloseReason]; known {
		return &domain.StatusReason{Reason: string(pe.CloseReason), Message: sentence}
	}
	return &domain.StatusReason{Reason: "closed", Message: "it closed before the platform kept why"}
}

// Describe sets on pe why it is in its status (StatusReason): on a draft where it stands with its
// planner, on an open one why it is not running yet and what reader does about it, on a running
// one whether its clock runs and whether a researcher is left to work, and on a closed one why it closed. It is the one place that says so.
// Before any of those, one that wants a machine of a launcher that is silent waits for that
// launcher (launcherUnreachable).
func (s *PlatformExperimentsService) Describe(ctx context.Context, pe *domain.PlatformExperiment, reader auth.Principal) error {
	var why *domain.StatusReason
	var signups []domain.AgentSignup
	var err error
	if pe.Status == domain.PlatformExpClosed {
		pe.StatusReason = closedReason(pe)
		return nil
	}
	// A draft's planner has a machine only while it plans.
	if pe.Status != domain.PlatformExpDraft || !pe.Answered() {
		if signups, err = s.store.ListAgentSignups(ctx, pe.ID); err != nil {
			return fmt.Errorf("platform_experiments.Describe: %w", err)
		}
	}
	switch {
	case pe.AwaitsRepository():
		why = &domain.StatusReason{Reason: "repository_creating", Message: "the platform is creating its repository on GitHub, and none of its machines starts before the repository is there"}
	case pe.Status == domain.PlatformExpDraft:
		why = draftReason(pe)
	case pe.Status == domain.PlatformExpRunning:
		why = runningReason(pe, signups)
	default:
		if why, err = s.waiting(ctx, pe, signups); err != nil {
			return err
		}
		why.Message = nextStep(why.Reason, signups, reader) + why.Message
	}
	silent, err := s.launcherUnreachable(ctx, pe, signups)
	if err != nil {
		return fmt.Errorf("platform_experiments.Describe: %w", err)
	}
	if silent != nil {
		why = silent
	}
	if why.WaitsFor, err = domain.WaitsFor(why.Reason, pe.CoordinatorHosted); err != nil {
		return fmt.Errorf("platform_experiments.Describe: %w", err)
	}
	pe.StatusReason = why
	return nil
}

// Answer is the body of every route that answers one platform experiment, so each of them says
// where it stands (Describe) as its reader is to read it.
func (h *PlatformExperimentsHandler) Answer(ctx context.Context, pe *domain.PlatformExperiment) (*struct{ Body *domain.PlatformExperiment }, error) {
	reader, err := h.caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.Describe(ctx, pe, reader); err != nil {
		return nil, auth.Internal(h.logger, "describe platform experiment", err, zap.String("id", pe.ID))
	}
	return &struct{ Body *domain.PlatformExperiment }{Body: pe}, nil
}

// start makes the open pe running and allocates each agent that shares the budget its quota. It
// answers the refusal of notStartable when pe is not to be started.
func (s *PlatformExperimentsService) start(ctx context.Context, pe *domain.PlatformExperiment, now time.Time) ([]*domain.AgentQuota, error) {
	// Read before the transaction, which must issue no query of its own.
	signups, err := s.store.ListAgentSignups(ctx, pe.ID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.start: %w", err)
	}
	runBudget, err := s.notStartable(ctx, pe, signups, now)
	if err != nil {
		return nil, err
	}
	started, quotas, err := s.store.StartPlatformExperimentTx(ctx, pe.ID, s.cfg.AgentsDue, func(participants []db.StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) {
		if len(participants) == 0 {
			return nil, nil, noResearchers(pe.ID)
		}
		researchers := 0
		for _, p := range participants {
			if p.Role == domain.SignupRoleCompetitor {
				researchers++
			}
		}
		// The ladder is decided here, from the roster the start transaction reads, and stored
		// with the start. Only its first stage's share of the budget is released now; each later
		// stage releases its own at the boundary it starts (see controller.applyCut), which stops
		// one agent exhausting the budget before the ladder has cut anyone.
		stages := domain.RunStages(pe.Stages, researchers)
		exploreFrac := stages[0].ReleaseFrac()
		// Every signed-up agent participates or the start fails. Skipping the ones that could not
		// be resolved used to mean a transient database error permanently disinherited an agent:
		// the experiment started without it, and there is no second allocation pass.
		bonuses := make([]float64, len(participants))
		totalBonusFraction := 0.0
		for i, p := range participants {
			if !p.AgentExists {
				return nil, nil, fmt.Errorf("platform_experiments.start: agent %s is signed up but does not exist", p.AgentID)
			}
			if p.HasTop3 {
				bonuses[i] = s.cfg.Top3BonusFraction
				totalBonusFraction += s.cfg.Top3BonusFraction
			}
		}

		allocated := make([]*domain.AgentQuota, 0, len(participants))
		for i, p := range participants {
			acceleratorGuaranteed, acceleratorBurst := domain.AllocateQuota(
				runBudget*exploreFrac, len(participants), bonuses[i], totalBonusFraction, s.cfg,
			)
			// The tier decides which column the share lands in, never how large it is: everyone
			// is allocated the same way and a burst-only participant's guaranteed part is moved
			// into burst, not taken away.
			acceleratorGuaranteed, acceleratorBurst = domain.ApplyQuotaTier(
				domain.ResolveQuotaTier(p.QuotaTierOverride, pe.DefaultQuotaTier), acceleratorGuaranteed, acceleratorBurst)
			id, err := domain.NewID(domain.AgentQuotaIDPrefix)
			if err != nil {
				return nil, nil, fmt.Errorf("platform_experiments.start: %w", err)
			}
			allocated = append(allocated, &domain.AgentQuota{
				ID:                         id,
				AgentID:                    p.AgentID,
				PlatformExperimentID:       pe.ID,
				GuaranteedAcceleratorHours: acceleratorGuaranteed,
				BurstAcceleratorHours:      acceleratorBurst,
				CreatedAt:                  now,
			})
		}
		return allocated, stages, nil
	})
	if err != nil {
		return nil, err
	}
	if !started {
		return nil, invalidTransition("platform experiment %s is no longer open", pe.ID)
	}
	for _, q := range quotas {
		s.logger.Info("quota allocated",
			zap.String("agentID", q.AgentID),
			zap.Float64("guaranteed_accelerator_hours", q.GuaranteedAcceleratorHours),
			zap.Float64("burst_accelerator_hours", q.BurstAcceleratorHours),
		)
	}
	return quotas, nil
}

// stored reads the platform experiment id as a write just left it.
func (s *PlatformExperimentsService) stored(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe == nil {
		return nil, noPlatformExperiment(id)
	}
	return pe, nil
}

func noPlatformExperiment(id string) error {
	return api.Refuse(http.StatusNotFound, "not_found", "no platform experiment "+id)
}

// SetSummary records the operator's narrative verdict on a run and answers the platform
// experiment with it. Unlike every other field, this is writable after the experiment closes: a
// summary only exists once the run is over.
func (s *PlatformExperimentsService) SetSummary(ctx context.Context, id, summary string) (*domain.PlatformExperiment, error) {
	err := s.store.SetPlatformExperimentSummary(ctx, id, summary)
	if errors.Is(err, db.ErrPlatformExperimentNotFound) {
		return nil, noPlatformExperiment(id)
	}
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.SetSummary: %w", err)
	}
	return s.stored(ctx, id)
}

// Close transitions a platform experiment that is not closed to closed, records top-3
// placements and answers it as closed. by is whose credential asked for it, nil when the
// platform closes it at its end time.
// topResults is a list of (agentID, finalMetric) ordered best-first (up to 3 entries). When it is
// empty the standings are derived from the metrics store instead: closing must never silently
// discard the outcome of a run just because the caller was a timer rather than a person.
func (s *PlatformExperimentsService) Close(ctx context.Context, id string, by *domain.Actor, topResults []AgentResult) (*domain.PlatformExperiment, error) {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe.Status == domain.PlatformExpClosed {
		return nil, invalidTransition("platform experiment %s is already closed", id)
	}
	// A draft never ran: closing it abandons the plan, and there are no standings to record.
	if pe.Status != domain.PlatformExpDraft {
		if len(topResults) == 0 {
			// Standings are what closing produces. Closing is irreversible, so deriving them failing
			// has to stop the close and be retried, not quietly close an experiment with no result.
			if topResults, err = s.derivedTopResults(ctx, id); err != nil {
				return nil, fmt.Errorf("close: derive standings for %s: %w", id, err)
			}
		}
		for i, r := range topResults {
			if i >= 3 {
				break
			}
			if err := s.store.RecordTop3(ctx, id, r.AgentID, r.FinalMetric); err != nil {
				return nil, fmt.Errorf("close: record placement for %s: %w", r.AgentID, err)
			}
		}
	}
	if err := s.store.ClosePlatformExperiment(ctx, id, by); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	return s.stored(ctx, id)
}

// SweepExpired closes every platform experiment whose ClosesAt has passed: a run at its end, one
// no researcher's machine came up for by its AgentsDueAt, a draft whose planning limit passed or
// whose answer no person took up, which releases its machine, and an open one that is not running
// by its BaselineDueAt: the machines that were to measure its baseline would be rented for nothing.
func (s *PlatformExperimentsService) SweepExpired(ctx context.Context) error {
	now := time.Now()
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpDraft, domain.PlatformExpOpen, domain.PlatformExpRunning} {
		pes, err := s.store.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("list %s platform experiments: %w", status, err)
		}
		for _, pe := range pes {
			closesAt := pe.ClosesAt()
			if closesAt.IsZero() || closesAt.After(now) {
				continue
			}
			if _, err := s.Close(ctx, pe.ID, nil, nil); err != nil {
				s.logger.Error("sweep_expired: close failed",
					zap.String("platform_experiment_id", pe.ID), zap.Error(err))
				continue
			}
			s.logger.Info("sweep_expired: closed platform experiment",
				zap.String("platform_experiment_id", pe.ID), zap.Time("closes_at", closesAt))
		}
	}
	return nil
}

// SweepAutoStart starts every open platform experiment that is to be started: its baseline is
// declared, its starts_at is absent or past, and it has a researcher and budget left for one.
// The request that declares the baseline or adds the researcher starts it itself (StartIfReady),
// so what the sweep brings is time: a starts_at that arrives. One with no baseline is given the
// job its baseline agents left, where the request they finished in could not declare it
// (DeclareMeasured). One that is not startable waits, saying why in its status_reason (Describe),
// until SweepExpired closes it.
func (s *PlatformExperimentsService) SweepAutoStart(ctx context.Context) error {
	now := time.Now().UTC()
	pes, err := s.store.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpOpen)
	if err != nil {
		return fmt.Errorf("list open platform experiments: %w", err)
	}
	for _, pe := range pes {
		if pe.BaselineJobID == "" {
			if err := s.DeclareMeasured(ctx, pe.ID); err != nil {
				s.logger.Error("sweep_auto_start: declare the measured baseline failed",
					zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			}
			continue
		}
		_, err := s.start(ctx, pe, now)
		var waiting *api.Refusal
		if errors.As(err, &waiting) {
			continue
		}
		if err != nil {
			s.logger.Error("sweep_auto_start: start failed",
				zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			continue
		}
		s.logger.Info("sweep_auto_start: started platform experiment", zap.String("platform_experiment_id", pe.ID))
	}
	return nil
}

// TellCoordinators records, for every open or running platform experiment whose coordinator the
// platform hosts, the status_reason it waits for that coordinator with, and none where it waits
// for someone else. A status_reason is derived from the roster and the clock and stored nowhere,
// so what moves it writes nothing a coordinator is woken by: a researcher's machine that stops
// with no restart left, a baseline agent's that does. The record is that write, and one that
// changes is announced as platform_experiment.status (db.TellCoordinator).
func (s *PlatformExperimentsService) TellCoordinators(ctx context.Context) error {
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		pes, err := s.store.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("list %s platform experiments: %w", status, err)
		}
		for _, pe := range pes {
			if !pe.CoordinatorHosted {
				continue
			}
			if err := s.tellCoordinator(ctx, pe); err != nil {
				s.logger.Error("tell_coordinators: failed", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
			}
		}
	}
	return nil
}

func (s *PlatformExperimentsService) tellCoordinator(ctx context.Context, pe *domain.PlatformExperiment) error {
	if err := s.Describe(ctx, pe, auth.Principal{}); err != nil {
		return err
	}
	reason := ""
	if pe.StatusReason.WaitsFor == domain.WaitsForCoordinator {
		reason = pe.StatusReason.Reason
	}
	return s.store.TellCoordinator(ctx, pe.ID, reason)
}

// StartExpirySweep runs SweepAutoStart, SweepExpired and TellCoordinators on a ticker until ctx
// is cancelled.
func (s *PlatformExperimentsService) StartExpirySweep(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.SweepAutoStart(ctx); err != nil {
				s.logger.Error("sweep_auto_start: scan", zap.Error(err))
			}
			if err := s.SweepExpired(ctx); err != nil {
				s.logger.Error("sweep_expired: scan", zap.Error(err))
			}
			if err := s.TellCoordinators(ctx); err != nil {
				s.logger.Error("tell_coordinators: scan", zap.Error(err))
			}
		}
	}
}
