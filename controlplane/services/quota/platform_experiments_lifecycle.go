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

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
	"go.uber.org/zap"
)

var ErrPlatformExperimentNotFound = errors.New("platform experiment not found")

// coordinatorReader reads a roster for the run's own bookkeeping, where no message allowance is shown.
var coordinatorReader = domain.Actor{Kind: domain.ActorCoordinator}

// checkLength holds a platform experiment to a run of at most quota.max_platform_experiment_days.
// Until its end it may rent machines.
func (s *PlatformExperimentsService) checkLength(durationHours float64) error {
	if durationHours <= 0 {
		return refuseUpdate(422, "invalid_length", "duration_hours is required and must be positive: how long the run lasts from the moment it starts")
	}
	if length := time.Duration(durationHours * float64(time.Hour)); length > s.cfg.MaxRunLength {
		return refuseUpdate(422, "run_too_long", "it would run %.1f days, and a platform experiment runs at most %g days (quota.max_platform_experiment_days): state a shorter duration_hours",
			length.Hours()/24, s.cfg.MaxRunLength.Hours()/24)
	}
	return nil
}

// withSettings returns spec with the platform's settings in the fields a request may leave to
// them: scheduler.default_report_interval_seconds, stages.default as a run of max_agents
// researchers has it (domain.RunStages) and quota.default_quota_tier.
func (s *PlatformExperimentsService) withSettings(spec CreatePlatformExperimentRequest) CreatePlatformExperimentRequest {
	if spec.ReportIntervalSeconds == 0 {
		spec.ReportIntervalSeconds = s.cfg.DefaultReportIntervalSeconds
	}
	if len(spec.Stages) == 0 {
		spec.Stages = domain.RunStages(s.cfg.DefaultStages, spec.MaxAgents)
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
// with the platform's settings where spec leaves a field to them, and refuses with every problem it finds.
func (s *PlatformExperimentsService) validate(spec CreatePlatformExperimentRequest, catalog domain.AgentTypeCatalog) (CreatePlatformExperimentRequest, error) {
	spec, err := trimTexts(s.withSettings(spec))
	if err != nil {
		return spec, err
	}
	var problems refusal.Problems
	refuse := func(reason, format string, args ...any) {
		problems = append(problems, refusal.Refuse(422, reason, fmt.Sprintf(format, args...)))
	}
	if spec.Name == "" || spec.Description == "" {
		refuse("invalid_experiment", "name and description are required: the description is the brief every agent works from")
	}
	if len(spec.Description) > domain.MaxProseBytes {
		refuse("invalid_experiment", "%s", tooLong("description", len(spec.Description)))
	}
	if err := domain.ValidateMetricDefinitions(spec.Metrics); err != nil {
		refuse("invalid_metrics", "metrics: %s", err)
	} else if err := domain.RequireRankingMetric(spec.Metrics); err != nil {
		refuse("invalid_metrics", "metrics: %s", err)
	}
	if spec.ReportIntervalSeconds <= 0 {
		refuse("invalid_experiment", "report_interval_seconds must be positive: how often a job really reports")
	}
	if err := domain.ValidateStages(spec.Stages); err != nil {
		refuse("invalid_stages", "stages: %s", err)
	}
	if tier := domain.QuotaTier(spec.DefaultQuotaTier); tier != domain.QuotaTierGuaranteed && tier != domain.QuotaTierBurstOnly {
		refuse("invalid_experiment", "default_quota_tier must be \"guaranteed\" or \"burst_only\"")
	}
	if spec.MaxConcurrentAccelerators != nil && *spec.MaxConcurrentAccelerators <= 0 {
		refuse("invalid_experiment", "max_concurrent_accelerators must be positive")
	}
	if spec.BudgetAcceleratorHours <= 0 {
		refuse("invalid_cost", "budget_accelerator_hours is required and must be positive: the cap on everything the run spends, in accelerator-hours, and in CPU core-hours for work on cores (hypothesisloop.dev/cpu=core costs 1 an hour per core)")
	}
	if spec.MaxAgents <= 0 {
		refuse("invalid_agents", "max_agents is required and must be positive: how many researchers (agents of role competitor) may be at work at once")
	} else if len(spec.Stages) > 1 && spec.MaxAgents < domain.MinResearchersForCut {
		refuse("invalid_stages", "stages: %d stages for at most %d researchers, and a run of fewer than %d has one stage since no boundary cuts: state one stage or leave stages out",
			len(spec.Stages), spec.MaxAgents, domain.MinResearchersForCut)
	}
	if spec.MaxAgents > 0 {
		if err := spec.Researchers.Check(spec.MaxAgents, catalog); err != nil {
			refuse("invalid_agents", "%s: researchers is the researchers it is to have at work, [{agent_type, model, hyperparameters, count}]", err)
		}
	}
	if err := problems.Add(s.checkLength(spec.DurationHours)); err != nil {
		return spec, err
	}
	return spec, problems.Err()
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
		return nil, refuseUpdate(422, "accelerator_unreachable", "%s", unreachableType(*sizedFor, reachable))
	}
	return clusters, nil
}

// unreachableType says that sizedFor is on none of the clusters of reachable, and which types are:
// the accelerator_type lines `hl capacity` prints, the only strings a proposal states.
func unreachableType(sizedFor domain.AcceleratorType, reachable map[string][]domain.Cluster) string {
	stated := fmt.Sprintf("accelerator_type %q is", sizedFor)
	if sizedFor == domain.CPUType {
		stated = fmt.Sprintf("work on CPU cores (no accelerator_type, or %q) is", domain.CPUType)
	}
	if len(reachable) == 0 {
		return stated + " on no cluster this org's jobs can be placed on now, and no cluster reports any type: register a cluster or turn a shared one on"
	}
	valid := make([]string, 0, len(reachable))
	for _, t := range slices.Sorted(maps.Keys(reachable)) {
		if t == string(domain.CPUType) {
			valid = append(valid, fmt.Sprintf("%q (or no accelerator_type, for work on cores)", t))
			continue
		}
		valid = append(valid, fmt.Sprintf("%q", t))
	}
	return stated + " on no cluster this org's jobs can be placed on now. State one of the types `hl capacity` lists now, copied verbatim: " + strings.Join(valid, ", ")
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
	catalog, err := s.store.ProjectAgentTypeCatalog(ctx, req.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	if req, err = s.validate(req, catalog); err != nil {
		return nil, err
	}
	if _, err := s.requireReachable(ctx, req.ProjectID, nil); err != nil {
		return nil, err
	}
	if req.CoordinatorHosted != (req.coordinator != nil) {
		return nil, fmt.Errorf("platform_experiments.Create: coordinator_hosted is %v and its model is resolved: %v", req.CoordinatorHosted, req.coordinator != nil)
	}
	if err := s.requireContinuable(ctx, req.ProjectID, req.repository.Named, req.Continues); err != nil {
		return nil, err
	}
	if err := s.requireContinuedMetrics(ctx, req.Continues, req.Metrics); err != nil {
		return nil, err
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
		Researchers:               req.Researchers,
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
		Continues:                 req.Continues,
		Actor:                     req.createdBy,
	}
	pe.BaselineDueAt = pe.StartsAt.Add(s.cfg.BaselineLimit)
	if err := s.store.CreatePlatformExperiment(ctx, pe, req.agents, req.signups); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	s.logger.Info("platform experiment created", zap.String("id", pe.ID), zap.String("name", pe.Name), zap.Int("agents", len(req.signups)))
	pe.SignupCount = len(req.signups)
	s.wakeRepositories(pe)
	return pe, nil
}

// requireContinuable refuses a run to continue that is not one: continues names a closed platform
// experiment of the project that keeps its code in repository and declared a baseline, whose
// commit the new baseline starts from. Empty continues starts fresh.
func (s *PlatformExperimentsService) requireContinuable(ctx context.Context, projectID, repository, continues string) error {
	if continues == "" {
		return nil
	}
	prior, err := s.store.GetPlatformExperiment(ctx, continues)
	if err != nil {
		return fmt.Errorf("platform_experiments: reading the run to continue: %w", err)
	}
	switch {
	case prior == nil || prior.RemovedAt != nil || prior.ProjectID != projectID:
		return refuseUpdate(422, "invalid_continues", "continues %q names no platform experiment of project %s", continues, projectID)
	case prior.Status != domain.PlatformExpClosed:
		return refuseUpdate(422, "invalid_continues", "continues %s, which is %s: a run carries on one that closed", continues, prior.Status)
	case prior.Repository == "" || prior.Repository != repository:
		return refuseUpdate(422, "invalid_continues", "continues %s, which keeps its code in %q: name that repository", continues, prior.Repository)
	case prior.BaselineJobID == "":
		return refuseUpdate(422, "invalid_continues", "continues %s, which declared no baseline: there is no harness to carry on", continues)
	}
	return nil
}

// requireContinuedMetrics refuses metrics that measure otherwise than the run continues names:
// a run that carries on another keeps its metric keys and their directions, or the two rank
// different things. Empty continues starts fresh and declares any.
func (s *PlatformExperimentsService) requireContinuedMetrics(ctx context.Context, continues string, metrics []domain.MetricDefinition) error {
	if continues == "" {
		return nil
	}
	prior, err := s.store.GetPlatformExperiment(ctx, continues)
	if err != nil {
		return fmt.Errorf("platform_experiments: reading the run continued: %w", err)
	}
	if prior == nil {
		return fmt.Errorf("platform_experiments: the run continued, %s, is gone", continues)
	}
	declared, continued := metricGoals(metrics), metricGoals(prior.Metrics)
	if declared != continued {
		return refuseUpdate(422, "invalid_metrics", "metrics: this run declares %s and continues %s, which measured %s: a run that continues another keeps its metric keys and their directions", declared, continues, continued)
	}
	return nil
}

// metricGoals names each metric's key and direction, sorted by key.
func metricGoals(metrics []domain.MetricDefinition) string {
	goals := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		goals = append(goals, metric.Key+" ("+metric.Direction+")")
	}
	if len(goals) == 0 {
		return "no metric"
	}
	slices.Sort(goals)
	return strings.Join(goals, ", ")
}

// coordinatorWanted says when the platform runs a hosted coordinator's machine (db.launcherCoordinatorsWhere).
const coordinatorWanted = "The coordinator's machine is asked for once the plan is accepted and the experiment is open, so it supervises the baseline and can change the lineup before the start, and is kept until the experiment closes."

// refuseUpdate is a create or a patch the caller can fix.
func refuseUpdate(status int, reason, format string, args ...any) error {
	return refusal.Refuse(status, reason, fmt.Sprintf(format, args...))
}

// Update changes the fields the patch carries and no others, and the experiment as patched has
// to pass validate. While the experiment is open every
// field of the patch can change. Once it runs, only the name, the description, duration_hours and
// max_agents can, and the baseline be raised: admission and the stage ladder have already acted on
// the rest. A new duration_hours moves the end of the run, counted from its start as before. A
// max_agents of a running one changes only the places not taken, and is held to the unspent hours
// of its researchers (seatsHoldTrials). A closed one, and a draft, which its
// planner's proposal fills, take no patch.
//
// While it runs, baseline_job_id with baseline_reason raises the baseline (raisedBaseline).
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
	if locked := patch.carried("name", "description", "duration_hours", "max_agents", "researchers", "baseline_job_id", "baseline_reason"); pe.Status == domain.PlatformExpRunning && len(locked) > 0 {
		return nil, refuseUpdate(409, "experiment_running", "only name, description, duration_hours, max_agents, researchers and the baseline (baseline_job_id with baseline_reason) change once a platform experiment runs, and the body also carries: %s. Leave them out", strings.Join(locked, ", "))
	}
	raising := pe.Status == domain.PlatformExpRunning && patch.BaselineJobID != nil
	if err := checkRaiseRequest(pe, patch); err != nil {
		return nil, err
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
	if patch.Researchers != nil {
		patched.Researchers = *patch.Researchers
	}
	// Held to the researchers at work in the write that stores it, the count a join is held to: a
	// running one loses the newest above it, and an open one refuses it (db.PoolBelowAtWorkError).
	// Researchers the patch does not state are capped to it there.
	if patch.MaxAgents != nil {
		patched.MaxAgents = *patch.MaxAgents
		if patch.Researchers == nil {
			patched.Researchers = patched.Researchers.Capped(patched.MaxAgents)
		}
		if pe.Status == domain.PlatformExpRunning && patched.MaxAgents > pe.MaxAgents {
			if err := s.seatsHoldTrials(ctx, &patched); err != nil {
				return nil, err
			}
		}
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
		patched.BaselineDueAt = patched.StartsAt.Add(pe.BaselineDueAt.Sub(pe.StartsAt))
	}
	if patch.DurationHours != nil {
		patched.DurationHours = *patch.DurationHours
		if pe.Status == domain.PlatformExpRunning {
			end := pe.StartsAt.Add(time.Duration(patched.DurationHours * float64(time.Hour)))
			if !end.After(time.Now()) {
				return nil, refuseUpdate(422, "invalid_length", "duration_hours %g ends the run at %s, which has passed: close it instead, or state a duration that ends later", patched.DurationHours, end.UTC().Format(time.RFC3339))
			}
			if !pe.StartedAt.IsZero() {
				patched.EndsAt = end
			} else {
				patched.EndsAt, patched.AgentsDueAt = end, minTime(pe.AgentsDueAt, end)
			}
		}
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
	catalog, err := s.store.AgentTypeCatalog(ctx, pe.ID)
	if err != nil {
		return nil, fmt.Errorf("platform_experiments.Update: %w", err)
	}
	checked, err := s.validate(CreatePlatformExperimentRequest{
		Name: patched.Name, Description: authored,
		BudgetAcceleratorHours: patched.BudgetAcceleratorHours, MaxAgents: patched.MaxAgents, Researchers: patched.Researchers,
		MaxConcurrentAccelerators: patched.MaxConcurrentAccelerators, Metrics: patched.Metrics,
		ReportIntervalSeconds: patched.ReportIntervalSeconds, StartsAt: patched.StartsAt, DurationHours: patched.DurationHours,
		Stages: patched.Stages, DefaultQuotaTier: string(patched.DefaultQuotaTier),
	}, catalog)
	if err != nil {
		return nil, err
	}
	patched.Name, patched.Description, patched.Metrics = checked.Name, checked.Description, checked.Metrics
	if patch.Metrics != nil {
		if err := s.requireContinuedMetrics(ctx, pe.Continues, patched.Metrics); err != nil {
			return nil, err
		}
	}
	if pe.Status == domain.PlatformExpOpen && (patch.BaselineJobID != nil || pe.BaselineJobID != "") {
		// The platform declares what its baseline agent measured, held to the agent's own
		// stronger jobs; a person or coordinator that declares chose among them.
		ownJobs := patch.BaselineJobID != nil && patch.by == nil
		if measured, err = s.heldToBaseline(ctx, &patched, ownJobs); err != nil {
			return nil, err
		}
	}
	var raise domain.BaselineRaise
	if raising {
		if raise, measured, err = s.raisedBaseline(ctx, pe, &patched, strings.TrimSpace(*patch.BaselineReason), patch.by); err != nil {
			return nil, err
		}
	}
	patched.Description = withMeasuredBaseline(patched.Description, measured)
	if raising {
		if err := s.store.RaiseBaseline(ctx, &patched, raise); err != nil {
			return nil, fmt.Errorf("platform_experiments.Update: %w", err)
		}
		s.logger.Info("baseline raised", zap.String("platform_experiment_id", pe.ID), zap.String("from", raise.From), zap.String("to", raise.To))
		return s.stored(ctx, pe.ID)
	}
	pe = &patched
	var researchers domain.ResearcherPool
	if patch.Researchers != nil {
		researchers = patched.Researchers
	}
	var below *db.PoolBelowAtWorkError
	switch err := s.store.UpdatePlatformExperiment(ctx, pe, expectedStatus, researchers); {
	case errors.As(err, &below) && below.Key == (domain.PoolKey{}):
		return nil, refuseUpdate(409, "max_agents_below_researchers", "max_agents %d is below the %d researchers (agents of role competitor) of platform experiment %s, which has not started: state at least %d, or remove researchers first. Once it runs, a lower max_agents cuts the newest of them", below.Count, below.AtWork, pe.ID, below.AtWork)
	case errors.As(err, &below):
		return nil, refuseUpdate(409, "researchers_below_at_work", "%s: a change of researchers takes out no researcher at work, so state at least that many", below)
	case err != nil:
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

// seatsHoldTrials refuses a max_agents raised on the running pe when what its researchers left
// unspent, split evenly over max_agents researchers, would not hold one trial of the baseline
// each: the share a researcher that joins is given (domain.LateShare) would not run one.
func (s *PlatformExperimentsService) seatsHoldTrials(ctx context.Context, pe *domain.PlatformExperiment) error {
	held, err := s.runningTrialHeld(ctx, pe)
	if err != nil || held == 0 {
		return err
	}
	quotas, err := s.researcherQuotas(ctx, pe)
	if err != nil {
		return fmt.Errorf("platform_experiments.Update: %w", err)
	}
	unspent := 0.0
	for _, quota := range quotas {
		unspent += quota.Unspent()
	}
	if each := unspent / float64(pe.MaxAgents); each < held {
		return refuseUpdate(422, "budget_below_baseline", "a trial holds %.3f accelerator-hours of its researcher's share (%g h, the estimate measured on the baseline job), and the researchers left %.3f unspent, %.3f for each of %d: state a max_agents that leaves each at least one trial", held, pe.JobEstimatedDurationHours, unspent, each, pe.MaxAgents)
	}
	return nil
}

// heldToBaseline holds pe, as it would be stored, to the job it names as its baseline: the job
// must have done what a baseline does (checkBaseline) and the budget must hold a trial of it for
// each researcher (budgetCoversTrials), and with ownJobs no other job of its agent beat it
// (strongerOwnJob). It sets on pe the estimate of a job that states none and answers the block
// the brief is given. It is the one check of a declaration, whoever declares.
func (s *PlatformExperimentsService) heldToBaseline(ctx context.Context, pe *domain.PlatformExperiment, ownJobs bool) (string, error) {
	baseline, err := s.baseline(ctx, pe, pe.BaselineJobID)
	if err != nil {
		return "", fmt.Errorf("platform_experiments: read baseline %s: %w", pe.BaselineJobID, err)
	}
	if err := checkBaseline(pe, baseline); err != nil {
		return "", err
	}
	if ownJobs {
		if err := s.strongerOwnJob(ctx, pe, baseline); err != nil {
			return "", err
		}
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
			if baseline == nil || job.Branch != domain.BaselineBranch(pe.ID) {
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
		return "", refusal.Refuse(http.StatusConflict, "baseline_undeclared", fmt.Sprintf("the baseline agents of platform experiment %s left %d COMPLETED jobs that report %s (%s), and the platform declares a job by itself only where there is one to declare: its coordinator declares the one the run is ranked against with %s",
			pe.ID, len(measured), ranked.Key, strings.Join(measured, ", "), declareBaseline(pe.ID)))
	case len(measuring) > 0:
		return "", refusal.Refuse(http.StatusConflict, "baseline_measuring", fmt.Sprintf("baseline agent %s is measuring the baseline of platform experiment %s: when it finishes the platform declares its COMPLETED job and starts the run, and nobody else has anything to do until then",
			strings.Join(measuring, ", "), pe.ID))
	case len(joined) == 0:
		return "", refusal.Refuse(http.StatusConflict, "baseline_required", fmt.Sprintf("platform experiment %s has no baseline declared and no agent of role baseline to measure one: its coordinator adds one with %s, and the platform declares the job it leaves and starts the run",
			pe.ID, addBaselineAgent(pe.ID)))
	}
	return "", refusal.Refuse(http.StatusConflict, "baseline_required", fmt.Sprintf("platform experiment %s has no baseline declared, and no baseline agent left a COMPLETED job that reports %s: %s. Its coordinator repairs what the brief can and adds another baseline agent with %s, and what only its owner can repair it writes into the summary",
		pe.ID, ranked.Key, strings.Join(gaveUp, "; "), addBaselineAgent(pe.ID)))
}

// DeclareMeasured declares the baseline of the open platform experiment id when its baseline
// agents left exactly one job to declare (measuredJob), by the declaration a coordinator makes
// (Update), which starts the run when nothing else is missing. The request in which a baseline
// agent finishes calls it, the one that changes what a declaration was refused for, and the
// sweep for what neither could do. A declaration that is refused stores nothing, and the
// experiment's status_reason says the refusal (undeclared) until it is cleared.
func (s *PlatformExperimentsService) DeclareMeasured(ctx context.Context, id string) error {
	return s.declareFinished(ctx, id, "")
}

// declareFinished is DeclareMeasured for the job a finishing baseline agent named, or for the
// one measuredJob finds when it named none.
func (s *PlatformExperimentsService) declareFinished(ctx context.Context, id, jobID string) error {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return err
	}
	if pe.Status != domain.PlatformExpOpen || pe.BaselineJobID != "" {
		return nil
	}
	var refused *refusal.Refusal
	if jobID == "" {
		signups, err := s.store.ListAgentSignups(ctx, id)
		if err != nil {
			return fmt.Errorf("platform_experiments.DeclareMeasured: %w", err)
		}
		jobID, err = s.measuredJob(ctx, pe, signups)
		if errors.As(err, &refused) {
			return nil
		}
		if err != nil {
			return err
		}
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
func (s *PlatformExperimentsService) undeclared(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup) (*refusal.Refusal, error) {
	jobID, err := s.measuredJob(ctx, pe, signups)
	var refused *refusal.Refusal
	if errors.As(err, &refused) {
		return refused, nil
	}
	if err != nil {
		return nil, err
	}
	declared := *pe
	declared.BaselineJobID = jobID
	if _, err := s.heldToBaseline(ctx, &declared, true); errors.As(err, &refused) {
		return refusal.Refuse(http.StatusConflict, refused.Reason, fmt.Sprintf("the platform did not declare job %s, the one job its baseline agents left, as the baseline of platform experiment %s: %s. The platform declares it in the request that clears this",
			jobID, pe.ID, refused.Message)), nil
	} else if err != nil {
		return nil, err
	}
	return refusal.Refuse(http.StatusConflict, "baseline_declaring", fmt.Sprintf("job %s is the baseline of platform experiment %s and the platform declares it within a minute", jobID, pe.ID)), nil
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
	var waiting *refusal.Refusal
	if errors.As(err, &waiting) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("platform_experiments.StartIfReady: %w", err)
	}
	s.logger.Info("started platform experiment", zap.String("platform_experiment_id", id))
	return nil
}

// maxAgentsReached refuses a researcher past max_agents, which counts the researchers at work and
// nothing else.
func maxAgentsReached(limit int) error {
	return refuseSignup(409, "max_agents_reached", "max_agents is %d and that many researchers (agents of role competitor) are at work. One that finished, was cut, was replaced or lost its machine with no restart left no longer counts, and agents of role baseline, reviewers and the coordinator never do: the owner raises max_agents, or the agent joins with another role", limit)
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
		return refusal.Refuse(http.StatusConflict, "experiment_closed", "platform experiment "+platformExpID+" is closed")
	}
	cut, err := s.store.IsAgentCut(ctx, platformExpID, agentID)
	if err != nil {
		return fmt.Errorf("platform_experiments.RequireActive: %w", err)
	}
	if cut {
		return refusal.Refuse(http.StatusConflict, "agent_held", "agent "+agentID+" was cut from platform experiment "+platformExpID)
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
		return 0, refusal.Refuse(http.StatusConflict, "baseline_required", fmt.Sprintf("platform experiment %s has no baseline declared, and the platform starts the run only once it has", pe.ID))
	}
	if pe.StartsAt.After(now) {
		return 0, refusal.Refuse(http.StatusConflict, "starts_at_ahead", fmt.Sprintf("platform experiment %s has its baseline and starts at its starts_at, %s", pe.ID, pe.StartsAt.UTC().Format(time.RFC3339)))
	}
	if !slices.ContainsFunc(signups, func(signup domain.AgentSignup) bool { return signup.SharesBudget() }) {
		return 0, noResearchers(pe.ID)
	}
	runBudget, err := s.RunBudget(ctx, pe)
	if err != nil {
		return 0, err
	}
	if runBudget <= 0 {
		return 0, refusal.Refuse(http.StatusConflict, "budget_spent", fmt.Sprintf("measuring the baseline spent the whole budget of %g accelerator hours, so nothing is left for the researchers to share: its owner raises budget_accelerator_hours", pe.BudgetAcceleratorHours))
	}
	return runBudget, nil
}

func noResearchers(id string) error {
	return refusal.Refuse(http.StatusConflict, "no_researchers", fmt.Sprintf("platform experiment %s has its baseline and no researcher to share its budget: add one with `hl platform-experiments agents add --hosted --platform-experiment %s` (POST /platform-experiments/%s/agents), and the platform starts the run", id, id, id))
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
	case pe.ProposedAt != nil && pe.AcceptsAt != nil:
		return &domain.StatusReason{Reason: "accepting", Message: "its planner proposed an experiment, in proposal, and the platform accepts it as proposed at accepts_at, with its baseline agent's machine starting now. Accept it sooner with POST /platform-experiments/" + pe.ID + "/accept, hold it to edit it with POST /platform-experiments/" + pe.ID + "/hold, revise it or close it"}
	case pe.ProposedAt != nil:
		return &domain.StatusReason{Reason: "proposed", Message: "its planner proposed an experiment, in proposal: accept it with POST /platform-experiments/" + pe.ID + "/accept, with answers to proposal.questions where it has any, revise it or close it. The platform closes it at proposal_expires_at"}
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
	var refused *refusal.Refusal
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

// nextStep is what an agent that reads an open pe at now does while it waits for reason, a
// sentence that ends in a space, or nothing when all it can do is wait. The reason's own sentence
// says what everyone else does. Every agent is told the run's clock with it, and an agent of role
// baseline its own.
func nextStep(pe *domain.PlatformExperiment, reason string, signups []domain.AgentSignup, reader auth.Principal, now time.Time) string {
	if reader.Kind != auth.KindAgent {
		return ""
	}
	own := slices.IndexFunc(signups, func(signup domain.AgentSignup) bool { return signup.AgentID == reader.AgentID })
	if own < 0 || signups[own].Role != domain.SignupRoleBaseline {
		return "Every job you submit is refused until the platform starts the run: prepare, then end your turn, and the start arrives as an update. " + runClock(pe)
	}
	step := ""
	if signups[own].FinishedAt == nil && reason == "baseline_measuring" {
		step = "Your job is admitted now: measure the baseline with one job (hl job submit), file its summary (hl job summary), then hl platform-experiments agents finish --baseline JOB, and the platform declares that job. Your quota is half the budget, for the measurement and any rerun of it (hl quota). "
	}
	return step + baselineClock(pe.BaselineDueAt, now) + runClock(pe)
}

// runClock is how long the run of an open pe lasts and when that counts from.
func runClock(pe *domain.PlatformExperiment) string {
	return fmt.Sprintf("The run lasts its duration_hours, %g, from the moment the platform starts it, once its baseline is declared: starts_at and ends_at are set then. ", pe.DurationHours)
}

// endsClock is when a running pe ends, and how long that is from now.
func endsClock(pe *domain.PlatformExperiment, now time.Time) string {
	return fmt.Sprintf("ends_at, %s, %d minutes from now", pe.EndsAt.UTC().Format(time.RFC3339), max(0, int(pe.EndsAt.Sub(now).Minutes())))
}

// lateBaselineJobs says of each of jobs, the jobs of an open platform experiment's baseline
// agents, that is late (lateBaselineJob): what its agent finishes on. Nothing when there is none.
func lateBaselineJobs(jobs []*domain.Experiment, due, now time.Time) string {
	var late []string
	for _, job := range jobs {
		if sentence := lateBaselineJob(job, due, now); sentence != "" {
			late = append(late, sentence)
		}
	}
	return strings.Join(late, "; ")
}

// lateBaselineJob says that job, a job of an open platform experiment's baseline agents, has not
// started running and, run from now for the estimated_duration_hours its agent stated, would end
// after due. Nothing when it would not.
func lateBaselineJob(job *domain.Experiment, due, now time.Time) string {
	if (job.Status != domain.StatusQueued && job.Status != domain.StatusSubmitted) || job.EstimatedDurationHours <= 0 {
		return ""
	}
	runs := time.Duration(job.EstimatedDurationHours * float64(time.Hour))
	if ends := now.Add(runs); !ends.After(due) {
		return ""
	}
	state := string(job.Status) + ", not running yet"
	if job.NotAdmittedReason != "" {
		state = string(job.Status) + " (" + job.NotAdmittedReason + ")"
	}
	return fmt.Sprintf("job %s is %s and its estimated_duration_hours is %g: started now it ends after baseline_due_at, %d minutes from now, so baseline_due_at cannot be met by it",
		job.ID, state, job.EstimatedDurationHours, max(0, int(due.Sub(now).Minutes())))
}

// waitsOnPlatform says job, a job of an open platform experiment's baseline, waits on the
// platform: for the environment its commit declares to be built, or placed on its cluster and
// not running yet, which includes pulling its image.
func waitsOnPlatform(job *domain.Experiment) bool {
	return job.Status == domain.StatusSubmitted || job.Status == domain.StatusQueued && job.NotAdmittedReason == domain.NotAdmittedEnvironmentBuilding
}

// baselineClock says when an open platform experiment whose baseline is due at due closes, how
// long that is from now, and what moves it.
func baselineClock(due, now time.Time) string {
	return fmt.Sprintf("baseline_due_at is %s, %d minutes from now: the platform closes it then if the run has not started. While a job of the baseline waits for the platform to build its environment or to start it on its cluster, baseline_due_at moves out by that time, at most %d minutes in all. ",
		due.UTC().Format(time.RFC3339), max(0, int(due.Sub(now).Minutes())), int(domain.MaxEnvironmentBuildTime.Minutes()))
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
func runningReason(pe *domain.PlatformExperiment, signups []domain.AgentSignup, now time.Time) *domain.StatusReason {
	switch {
	case pe.AwaitsAgents():
		return &domain.StatusReason{Reason: "agents_starting", Message: "the run started at starts_at, " + pe.StartsAt.UTC().Format(time.RFC3339) + ", and its researchers' machines are starting: the first one running sets started_at. The platform closes it at agents_due_at, with close_reason " + string(domain.CloseAgentsNotStarted) + ", if none is running by then, and at " + endsClock(pe, now) + ", in any case"}
	case !researcherLeft(signups):
		return &domain.StatusReason{Reason: "researchers_done", Message: fmt.Sprintf("every researcher of platform experiment %s has finished, was cut or lost its machine with no restart left, so nobody is left to submit a job. Where it runs them the platform adds a researcher of its researchers in the place of one that finished or lost its machine while the run has time for a trial and its budget a share for one, and none is added now; its coordinator may add one up to max_agents with `hl platform-experiments agents add --hosted --platform-experiment %s`. The platform closes it at its %s, and its coordinator may write the summary with `hl platform-experiments summary --file FILE --platform-experiment %s` and close it sooner with `hl platform-experiments close --platform-experiment %s`",
			pe.ID, pe.ID, endsClock(pe, now), pe.ID, pe.ID)}
	}
	return &domain.StatusReason{Reason: "running", Message: "its researchers are at work and the platform closes it at " + endsClock(pe, now)}
}

// closedReason is why the closed pe closed: its close_reason and what that means. Of one closed
// before its run started for want of a baseline, it says whether a job was submitted for one.
func (s *PlatformExperimentsService) closedReason(ctx context.Context, pe *domain.PlatformExperiment) (*domain.StatusReason, error) {
	closer := "the owner"
	if (pe.CloseReason == domain.CloseByRequest || pe.CloseReason == domain.CloseRemoved) && pe.ClosedBy != nil {
		if err := s.store.NameActors(ctx, []*domain.Actor{pe.ClosedBy}); err != nil {
			return nil, fmt.Errorf("platform_experiments.closedReason: %w", err)
		}
		closer = pe.ClosedBy.Name
	}
	how := map[domain.CloseReason]string{
		domain.CloseEndTimeReached:       "the run reached its ends_at and the platform closed it",
		domain.CloseByRequest:            closer + " asked for it to be closed",
		domain.CloseRemoved:              closer + " removed it",
		domain.ClosePlanningLimitReached: "its planner answered nothing by planning_ends_at and the platform closed the draft",
		domain.CloseProposalExpired:      "no person accepted or revised its planner's answer by proposal_expires_at and the platform closed the draft",
		domain.ClosePlannerStopped:       "its planner's machine ended before it answered and the platform had restarted it as often as it restarts one, so it closed the draft: the planner's instance in the roster says how the last machine ended (end_reason, exit_log)",
		domain.CloseAgentsNotStarted:     "it started and no researcher's machine was running by agents_due_at, so the platform closed it. Each agent's instance says why its machine did not come up",
		domain.CloseRepositoryNotCreated: "the platform could not create its repository, so none of its machines started. GitHub answered: " + pe.RepositoryError,
	}
	if pe.CloseReason == domain.CloseBaselineLimitReached {
		submitted, err := s.jobsSubmitted(ctx, pe.ID)
		if err != nil {
			return nil, err
		}
		how[pe.CloseReason] = baselineLimitReached(pe, submitted)
	}
	if pe.CloseMessage != "" {
		how[domain.CloseByRequest] += `, saying "` + strings.Join(strings.Fields(pe.CloseMessage), " ") + `"`
	}
	if sentence, known := how[pe.CloseReason]; known {
		return &domain.StatusReason{Reason: string(pe.CloseReason), Message: sentence}, nil
	}
	return &domain.StatusReason{Reason: "closed", Message: "it closed before the platform kept why"}, nil
}

// baselineLimitReached is why pe, closed at its baseline_due_at with submitted jobs, never ran.
// An open platform experiment admits the jobs of its baseline agents alone.
func baselineLimitReached(pe *domain.PlatformExperiment, submitted int) string {
	const closed = "it was not running by baseline_due_at and the platform closed it: "
	switch {
	case pe.BaselineJobID != "":
		return closed + "its baseline, job " + pe.BaselineJobID + ", was declared and the start still waited for a researcher, or for budget left to share among them"
	case submitted == 0:
		return closed + "no job was ever submitted to measure its baseline"
	}
	return closed + fmt.Sprintf("%d jobs were submitted to measure its baseline and none was declared: the roster says how each baseline agent ended", submitted)
}

// jobsSubmitted is how many jobs the platform experiment id took, by every agent.
func (s *PlatformExperimentsService) jobsSubmitted(ctx context.Context, id string) (int, error) {
	jobs, err := s.store.AgentJobsByAgent(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("platform_experiments.jobsSubmitted: %w", err)
	}
	total := 0
	for _, agent := range jobs {
		total += agent.Submitted
	}
	return total, nil
}

// Describe sets on pe why it is in its status (StatusReason): on a draft where it stands with its
// planner, on an open one why it is not running yet and what reader does about it, on a running
// one whether its clock runs and whether a researcher is left to work, and on a closed one why it closed. It is the one place that says so.
func (s *PlatformExperimentsService) Describe(ctx context.Context, pe *domain.PlatformExperiment, reader auth.Principal) error {
	var why *domain.StatusReason
	var signups []domain.AgentSignup
	var err error
	if pe.Status == domain.PlatformExpClosed {
		if why, err = s.closedReason(ctx, pe); err != nil {
			return fmt.Errorf("platform_experiments.Describe: %w", err)
		}
		if err := summarize(why, pe, nil, time.Now()); err != nil {
			return fmt.Errorf("platform_experiments.Describe: %w", err)
		}
		pe.StatusReason = why
		return nil
	}
	// A draft's planner has a machine only while it plans.
	if pe.Status != domain.PlatformExpDraft || !pe.Answered() {
		if signups, err = s.store.ListAgentSignups(ctx, pe.ID); err != nil {
			return fmt.Errorf("platform_experiments.Describe: %w", err)
		}
	}
	now := time.Now().UTC()
	switch {
	case pe.AwaitsRepository():
		why = &domain.StatusReason{Reason: "repository_creating", Message: "the platform is creating its repository on GitHub, and none of its machines starts before the repository is there"}
	case pe.Status == domain.PlatformExpDraft:
		why = draftReason(pe)
	case pe.Status == domain.PlatformExpRunning:
		why = runningReason(pe, signups, now)
	default:
		if why, err = s.waiting(ctx, pe, signups); err != nil {
			return err
		}
		if why.Reason == "baseline_measuring" {
			jobs, err := s.store.ListExperimentsTesting(ctx, []string{pe.ID})
			if err != nil {
				return fmt.Errorf("platform_experiments.Describe: %w", err)
			}
			if late := lateBaselineJobs(jobs, pe.BaselineDueAt, now); late != "" {
				why.Message += ". " + late
			}
		}
		why.Message = nextStep(pe, why.Reason, signups, reader, now) + why.Message
	}
	if err := summarize(why, pe, signups, now); err != nil {
		return fmt.Errorf("platform_experiments.Describe: %w", err)
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
	atWork, err := h.svc.store.ResearchersAtWork(ctx, pe.ID)
	if err != nil {
		return nil, auth.Internal(h.logger, "count researchers at work", err, zap.String("id", pe.ID))
	}
	counts := pe.Researchers.AtWorkOf(atWork)
	pe.ResearchersAtWork = &counts
	if pe.Repository != "" {
		if h.proxyURL == "" {
			return nil, auth.Internal(h.logger, "describe platform experiment", errors.New("no git proxy address to name its repository at"), zap.String("id", pe.ID))
		}
		pe.GitURL = domain.GitProxyURL(h.proxyURL, pe.ID)
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
				p.QuotaTier, acceleratorGuaranteed, acceleratorBurst)
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

// tooLong says why a description or a summary of size bytes is refused.
func tooLong(field string, size int) string {
	return fmt.Sprintf("%s is %d bytes and at most %d are kept, since every read of the platform experiment answers it whole: shorten it, and keep the detail in the repository and name the path", field, size, domain.MaxProseBytes)
}

func noPlatformExperiment(id string) error {
	return refusal.Refuse(http.StatusNotFound, "not_found", "no platform experiment "+id)
}

// SetSummary records the operator's narrative verdict on a run and answers the platform
// experiment with it. Unlike every other field, this is writable after the experiment closes: a
// summary only exists once the run is over. The platform's record of the close is kept beside it
// and answered after it, so a copy of the record read back with the summary is dropped.
func (s *PlatformExperimentsService) SetSummary(ctx context.Context, id, summary string) (*domain.PlatformExperiment, error) {
	if i := strings.Index(summary, closeRecordHeading); i >= 0 {
		summary = strings.TrimRight(summary[:i], " \n")
	}
	if len(summary) > domain.MaxProseBytes {
		return nil, refuseUpdate(422, "summary_too_long", "%s", tooLong("summary", len(summary)))
	}
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
// platform closes it at its end time, and message what that caller said of why.
// topResults is a list of (agentID, finalMetric) ordered best-first (up to 3 entries). When it is
// empty the standings are derived from the metrics store instead: closing must never silently
// discard the outcome of a run just because the caller was a timer rather than a person.
func (s *PlatformExperimentsService) Close(ctx context.Context, id string, by *domain.Actor, message string, topResults []AgentResult) (*domain.PlatformExperiment, error) {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	if pe.Status == domain.PlatformExpClosed {
		return nil, invalidTransition("platform experiment %s is already closed", id)
	}
	return s.close(ctx, pe, topResults, func() error {
		return s.store.ClosePlatformExperiment(ctx, id, by, domain.EventDetail(message))
	})
}

// Remove removes platform experiment id for by: it is answered to nobody from then on, and
// everything it holds is kept, its spend included. One not closed yet is closed as Close closes
// it, its agents stopped and its jobs evicted, with close_reason removed, in the write that
// removes it.
func (s *PlatformExperimentsService) Remove(ctx context.Context, id string, by domain.Actor) error {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return err
	}
	remove := func() error {
		err := s.store.RemovePlatformExperiment(ctx, id, by)
		if errors.Is(err, db.ErrPlatformExperimentNotFound) {
			return noPlatformExperiment(id)
		}
		return err
	}
	if pe.Status == domain.PlatformExpClosed {
		err = remove()
	} else {
		_, err = s.close(ctx, pe, nil, remove)
	}
	if err != nil {
		return err
	}
	s.logger.Info("platform experiment removed", zap.String("platform_experiment_id", id))
	return nil
}

// close records the standings and the agent lines of pe, which is not closed, runs write, which
// closes it, and writes the record of the close once its every job settled.
func (s *PlatformExperimentsService) close(ctx context.Context, pe *domain.PlatformExperiment, topResults []AgentResult, write func() error) (*domain.PlatformExperiment, error) {
	id := pe.ID
	var err error
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
	if pe.Status != domain.PlatformExpDraft {
		lines, err := s.closeAgents(ctx, pe, time.Now())
		if err != nil {
			return nil, fmt.Errorf("close: %w", err)
		}
		if err := s.store.SetCloseAgents(ctx, id, lines); err != nil {
			return nil, fmt.Errorf("close: %w", err)
		}
	}
	if err := write(); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	closed, err := s.stored(ctx, id)
	if err != nil || pe.Status == domain.PlatformExpDraft {
		return closed, err
	}
	// A job still live at the close is billed when it ends, and a record written before would
	// state less than consumed_accelerator_hours: RecordCloses writes it once every job settled.
	settled, err := s.store.JobsSettled(ctx, id)
	if err != nil || !settled {
		return closed, err
	}
	if err := s.recordClose(ctx, closed); err != nil {
		return nil, fmt.Errorf("close: %w", err)
	}
	return s.stored(ctx, id)
}

// Reopen sets the closed platform experiment id, whose run started, running again until endsAt as
// the same run: its registry, jobs, branches, brief, baseline and agents stay, and what the close
// wrote goes. Its agents and coordinator are live again by the rows alone: those whose run ended
// only with the close have no end of their own, while one that finished or was cut before keeps
// it. endsAt is held to the length a create is held to, counted from the run's start.
// coordinatorModel, resolved by the caller, moves a hosted coordinator to that model, and empty
// keeps the one stored.
func (s *PlatformExperimentsService) Reopen(ctx context.Context, id string, endsAt time.Time, coordinatorModel string) (*domain.PlatformExperiment, error) {
	pe, err := s.stored(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case pe.Status != domain.PlatformExpClosed:
		return nil, invalidTransition("platform experiment %s is %s: only a closed one is reopened", id, pe.Status)
	case pe.StartedAt.IsZero():
		return nil, invalidTransition("platform experiment %s closed before its run started (close_reason %s): there is no run to continue, create it again", id, pe.CloseReason)
	case !endsAt.After(time.Now()):
		return nil, refuseUpdate(422, "invalid_length", "ends_at %s has passed: state a time ahead", endsAt.UTC().Format(time.RFC3339))
	case coordinatorModel != "" && !pe.CoordinatorHosted:
		return nil, refuseSignup(400, "invalid_request", "coordinator_model is set and platform experiment %s has no hosted coordinator", id)
	}
	if length := endsAt.Sub(pe.StartsAt); length > s.cfg.MaxRunLength {
		return nil, refuseUpdate(422, "run_too_long", "ends_at %s would have the run, started at %s, last %.1f days, and a platform experiment runs at most %g days (quota.max_platform_experiment_days): state an earlier ends_at",
			endsAt.UTC().Format(time.RFC3339), pe.StartsAt.UTC().Format(time.RFC3339), length.Hours()/24, s.cfg.MaxRunLength.Hours()/24)
	}
	durationHours := endsAt.Sub(pe.StartsAt).Hours()
	err = s.store.ReopenPlatformExperiment(ctx, id, endsAt, durationHours, coordinatorModel)
	switch {
	case errors.Is(err, db.ErrPlatformExperimentNotFound):
		return nil, noPlatformExperiment(id)
	case errors.Is(err, db.ErrNotReopenable):
		return nil, invalidTransition("platform experiment %s is continued by a later run, or changed since it was read: a continued run stays closed", id)
	case err != nil:
		return nil, fmt.Errorf("reopen: %w", err)
	}
	s.logger.Info("platform experiment reopened", zap.String("platform_experiment_id", id), zap.Time("ends_at", endsAt))
	return s.stored(ctx, id)
}

// closeRecordWindow is how long after its close a platform experiment whose jobs have not all
// settled waits for its record of the close.
const closeRecordWindow = 24 * time.Hour

// RecordCloses writes the record of the close of every platform experiment Close left without
// one, because a job of it was still live, once its every job ended and was settled.
func (s *PlatformExperimentsService) RecordCloses(ctx context.Context) error {
	ids, err := s.store.ClosesToRecord(ctx, closeRecordWindow)
	if err != nil {
		return fmt.Errorf("closes to record: %w", err)
	}
	for _, id := range ids {
		pe, err := s.stored(ctx, id)
		if err == nil {
			err = s.recordClose(ctx, pe)
		}
		if err != nil {
			s.logger.Error("record_closes: failed", zap.String("platform_experiment_id", id), zap.Error(err))
		}
	}
	return nil
}

// closeRecordHeading opens the platform's record of a close.
const closeRecordHeading = "Recorded by the platform:"

// recordClose writes beside the summary of pe, which closed after it was open and whose every
// job ended and was settled, what the platform knows of it: why it closed, the jobs submitted,
// each agent's last activity and how its run ended, and the spend, which is then
// consumed_accelerator_hours whole. It is answered after the coordinator's summary, which a
// coordinator on its last turn still rewrites, so an interim summary never stands alone.
func (s *PlatformExperimentsService) recordClose(ctx context.Context, pe *domain.PlatformExperiment) error {
	why, err := s.closedReason(ctx, pe)
	if err != nil {
		return err
	}
	jobs, err := s.store.AgentJobsByAgent(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("recordClose: %w", err)
	}
	signups, err := s.store.ListAgentSignups(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("recordClose: %w", err)
	}
	agents, err := s.store.CloseAgents(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("recordClose: %w", err)
	}
	if agents == "" {
		return fmt.Errorf("recordClose: %s has no agent lines taken at its close", pe.ID)
	}
	if err := s.Spend(ctx, pe); err != nil {
		return err
	}
	submitted := 0
	for _, agent := range jobs {
		submitted += agent.Submitted
	}
	var record strings.Builder
	fmt.Fprintf(&record, "%s it closed at %s.\n\n", closeRecordHeading, pe.ClosedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&record, "Close reason: %s: %s.\n", why.Reason, why.Message)
	fmt.Fprintf(&record, "Jobs submitted: %d.\n", submitted)
	fmt.Fprintf(&record, "Spent: %g of the budget of %g accelerator hours.\n\n%s", *pe.ConsumedAcceleratorHours, pe.BudgetAcceleratorHours, agents)
	if len(pe.BaselineRaises) > 0 {
		fmt.Fprintf(&record, "\nBaseline raised while it ran:\n")
		for _, raise := range pe.BaselineRaises {
			by := string(raise.Actor.Kind)
			if raise.Actor.ID != "" {
				by += " " + raise.Actor.ID
			}
			fmt.Fprintf(&record, "- at %s, from job %s to job %s, by %s: %s.\n", raise.At.UTC().Format(time.RFC3339), raise.From, raise.To, by, raise.Reason)
		}
	}
	if err := s.messagesSent(ctx, pe.ID, signups, &record); err != nil {
		return fmt.Errorf("recordClose: %w", err)
	}
	if pe.CloseReason == domain.CloseBaselineLimitReached {
		if err := s.baselineLeft(ctx, pe, &record); err != nil {
			return fmt.Errorf("recordClose: %w", err)
		}
	}
	return s.store.RecordClose(ctx, pe.ID, record.String())
}

// messagesSent writes to record every message sent to an agent of the platform experiment.
func (s *PlatformExperimentsService) messagesSent(ctx context.Context, id string, signups []domain.AgentSignup, record *strings.Builder) error {
	messages, err := s.store.ListAgentMessages(ctx, id, "")
	if err != nil || len(messages) == 0 {
		return err
	}
	names := make(map[string]string, len(signups))
	for _, signup := range signups {
		names[signup.AgentID] = signup.Name
	}
	fmt.Fprintf(record, "\nMessages to agents:\n")
	for _, m := range messages {
		to := names[m.AgentID]
		if to == "" {
			to = m.AgentID
		}
		fmt.Fprintf(record, "- at %s, from the %s to %s: %s\n", m.At.UTC().Format(time.RFC3339), m.From, to, strings.Join(strings.Fields(m.Text), " "))
	}
	return nil
}

// baselineLeft writes to record what the baseline agents of pe, closed before its run started,
// left for another run to start from: each job with how it ended, the commit it ran and the
// environment built for it, and the last commit of the baseline branch a job was submitted from.
func (s *PlatformExperimentsService) baselineLeft(ctx context.Context, pe *domain.PlatformExperiment, record *strings.Builder) error {
	jobs, err := s.store.ListExperimentsTesting(ctx, []string{pe.ID})
	if err != nil {
		return err
	}
	branch := domain.BaselineBranch(pe.ID)
	fmt.Fprintf(record, "\nWhat the baseline left, on branch %s:\n", branch)
	last := ""
	for _, job := range jobs {
		fmt.Fprintf(record, "- job %s: %s", job.ID, domain.AnsweredStatus(job.Status, job.EvictionReason))
		if job.EvictionReason != "" {
			fmt.Fprintf(record, " (%s)", job.EvictionReason)
		}
		if job.InfraRequeueCount > 0 {
			fmt.Fprintf(record, "; requeued %d times for faults of the platform's, the last %s", job.InfraRequeueCount, job.InfraRequeueReason)
		}
		fmt.Fprintf(record, "; code %s", job.CodeRef)
		if job.EnvironmentKey != "" {
			env, err := s.environments.Get(ctx, job.EnvironmentKey)
			if err != nil {
				return fmt.Errorf("environment of %s: %w", job.ID, err)
			}
			switch {
			case env == nil:
				return fmt.Errorf("job %s names environment %s, which does not exist", job.ID, job.EnvironmentKey)
			case env.State == domain.EnvironmentReady:
				fmt.Fprintf(record, "; its environment %s built as %s", domain.EnvironmentFile, env.Image)
			default:
				fmt.Fprintf(record, "; its environment %s %s, not built", domain.EnvironmentFile, env.State)
			}
		}
		record.WriteString(".\n")
		if job.Branch == branch {
			last = job.CodeRef
		}
	}
	switch {
	case len(jobs) == 0:
		record.WriteString("No job.\n")
	case last != "":
		fmt.Fprintf(record, "The last commit of %s a job was submitted from: %s.\n", branch, last)
	}
	return nil
}

// closeAgents is the agent lines of the record of the close of pe, as its agents stand at
// closedAt, the moment just before the close is written: once the record is written, after every
// job settled, their machines are released and what their agents last said is gone.
func (s *PlatformExperimentsService) closeAgents(ctx context.Context, pe *domain.PlatformExperiment, closedAt time.Time) (string, error) {
	jobs, err := s.store.AgentJobsByAgent(ctx, pe.ID)
	if err != nil {
		return "", err
	}
	signups, err := s.store.ListAgentSignups(ctx, pe.ID)
	if err != nil {
		return "", err
	}
	var lines strings.Builder
	lines.WriteString("Agents:\n")
	if pe.Coordinator != nil {
		ended := runEnded(&pe.Coordinator.Instance, nil, "", closedAt)
		if pe.Coordinator.Instance.AskedAt == nil {
			ended += ": the platform launches a coordinator once the run starts or the experiment waits for it, and this one closed before either, so this record is its summary"
		}
		fmt.Fprintf(&lines, "- coordinator: %s; %s.\n", lastActivity(pe.Coordinator.Activity), ended)
	}
	for _, signup := range signups {
		if signup.Role == domain.SignupRolePlanner {
			continue
		}
		fmt.Fprintf(&lines, "- %s (%s): %d jobs submitted; %s; %s.\n", signup.Name, signup.Role, jobs[signup.AgentID].Submitted,
			lastActivity(signup.Activity), runEnded(signup.Instance, signup.FinishedAt, signup.FinishedReason, closedAt))
	}
	return lines.String(), nil
}

// lastActivity is what an agent last said it was doing, as a close record states it.
func lastActivity(activity *domain.AgentActivity) string {
	if activity == nil {
		return "no activity reported"
	}
	return fmt.Sprintf("last activity %q since %s", activity.Text, activity.At.UTC().Format(time.RFC3339))
}

// runEnded is how an agent's run stood at closedAt, the close: finished for its reason, never
// launched when no machine was asked for it, at work when it held a running machine or one that
// ended only with the close, and otherwise what its machine was doing. An end is reported only of
// a machine that ended before the close and was not replaced: EndReason is why the last machine
// ended, which a healthy replacement leaves standing.
func runEnded(instance *domain.AgentInstance, finishedAt *time.Time, finishedReason string, closedAt time.Time) string {
	switch {
	case finishedAt != nil:
		return fmt.Sprintf("finished at %s: %q", finishedAt.UTC().Format(time.RFC3339), finishedReason)
	case instance == nil || instance.AskedAt == nil:
		return "never launched"
	case instance.Name == "" && instance.ExitedAt != nil && !instance.ExitedAt.Before(closedAt),
		instance.Name != "" && instance.Shown() == domain.InstanceRunning:
		return "did not finish: at work at the close"
	case instance.Name == "" && instance.EndReason != "":
		ended := fmt.Sprintf("did not finish: its machine ended (%s)", instance.EndReason)
		if instance.ExitedAt != nil {
			ended += " at " + instance.ExitedAt.UTC().Format(time.RFC3339)
		}
		if instance.Stopped {
			ended += " with no restart left"
		}
		return ended + ", and none ran after it"
	}
	return "did not finish: its machine was " + instance.Shown() + " at the close"
}

// SweepExpired closes every platform experiment whose ClosesAt has passed: a run at its end, one
// no researcher's machine came up for by its AgentsDueAt, a draft whose planning limit passed or
// whose answer no person took up, which releases its machine, and an open one that is not running
// by its BaselineDueAt: the machines that were to measure its baseline would be rented for nothing.
func (s *PlatformExperimentsService) SweepExpired(ctx context.Context) error {
	return s.sweepExpired(ctx, domain.PlatformExpDraft, domain.PlatformExpOpen, domain.PlatformExpRunning)
}

// SweepEnded is SweepExpired for running platform experiments alone: a run's end is what its
// agents and its coordinator's last turn are timed by, so it is closed on time.
func (s *PlatformExperimentsService) SweepEnded(ctx context.Context) error {
	return s.sweepExpired(ctx, domain.PlatformExpRunning)
}

func (s *PlatformExperimentsService) sweepExpired(ctx context.Context, statuses ...domain.PlatformExperimentStatus) error {
	now := time.Now()
	var failed error
	for _, status := range statuses {
		pes, err := s.store.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("list %s platform experiments: %w", status, err))
			continue
		}
		for _, pe := range pes {
			closesAt := pe.ClosesAt()
			if closesAt.IsZero() || closesAt.After(now) {
				continue
			}
			if _, err := s.Close(ctx, pe.ID, nil, "", nil); err != nil {
				s.logger.Error("sweep_expired: close failed",
					zap.String("platform_experiment_id", pe.ID), zap.Error(err))
				continue
			}
			s.logger.Info("sweep_expired: closed platform experiment",
				zap.String("platform_experiment_id", pe.ID), zap.Time("closes_at", closesAt))
		}
	}
	return failed
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
		var waiting *refusal.Refusal
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

// TendBaselines keeps the clock of every open platform experiment with no baseline declared.
// Time the platform spends on a job of its baseline, building its environment or starting it on
// its cluster, is the platform's and not the baseline agent's: baseline_due_at moves out by it,
// at most domain.MaxEnvironmentBuildTime in all (db.HoldBaselineClock). And the agent of a job
// that, started now, ends after baseline_due_at is told so once, as job.held, which wakes it to
// act on it where status_reason alone would wait to be read.
func (s *PlatformExperimentsService) TendBaselines(ctx context.Context) error {
	pes, err := s.store.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpOpen)
	if err != nil {
		return fmt.Errorf("list open platform experiments: %w", err)
	}
	for _, pe := range pes {
		if pe.BaselineJobID != "" {
			continue
		}
		if err := s.tendBaseline(ctx, pe); err != nil {
			s.logger.Error("tend_baselines: failed", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
		}
	}
	return nil
}

func (s *PlatformExperimentsService) tendBaseline(ctx context.Context, pe *domain.PlatformExperiment) error {
	jobs, err := s.store.ListExperimentsTesting(ctx, []string{pe.ID})
	if err != nil {
		return err
	}
	held := slices.ContainsFunc(jobs, waitsOnPlatform)
	if err := s.store.HoldBaselineClock(ctx, pe.ID, held, domain.MaxEnvironmentBuildTime); err != nil {
		return err
	}
	if held {
		if pe, err = s.stored(ctx, pe.ID); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	var failed error
	for _, job := range jobs {
		late := lateBaselineJob(job, pe.BaselineDueAt, now)
		if late == "" {
			continue
		}
		reason := domain.HeldBaselineLate + ": " + late + ". " + strings.TrimSpace(baselineClock(pe.BaselineDueAt, now))
		if err := s.store.MarkBaselineLate(ctx, job.ID, reason); err != nil {
			failed = errors.Join(failed, fmt.Errorf("job %s: %w", job.ID, err))
		}
	}
	return failed
}

// TellCoordinators records, for every open or running platform experiment whose coordinator the
// platform hosts, the status_reason it waits for that coordinator with, and none where it waits
// for someone else. A status_reason is derived from the roster and the clock and stored nowhere,
// so what moves it writes nothing a coordinator is woken by: a researcher's machine that stops
// with no restart left, a baseline agent's that does. The record is that write, and one that
// changes is announced as platform_experiment.status (db.TellCoordinator).
func (s *PlatformExperimentsService) TellCoordinators(ctx context.Context) error {
	var failed error
	for _, status := range []domain.PlatformExperimentStatus{domain.PlatformExpOpen, domain.PlatformExpRunning} {
		pes, err := s.store.ListPlatformExperimentsByStatus(ctx, status)
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("list %s platform experiments: %w", status, err))
			continue
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
	return failed
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

// StartExpirySweep runs SweepAutoStart, TendBaselines, SweepExpired, RecordCloses,
// TellCoordinators and the idle notices on a ticker until ctx is cancelled, each pass bounded by
// passTimeout. TendBaselines runs before SweepExpired, so a baseline that waited on the platform
// until its deadline is given that time before it is closed.
func (s *PlatformExperimentsService) StartExpirySweep(ctx context.Context, interval, passTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(ctx, passTimeout)
			s.expirySweep(passCtx)
			cancel()
		}
	}
}

func (s *PlatformExperimentsService) expirySweep(ctx context.Context) {
	if err := s.SweepAutoStart(ctx); err != nil {
		s.logger.Error("sweep_auto_start: scan", zap.Error(err))
	}
	if err := s.TendBaselines(ctx); err != nil {
		s.logger.Error("tend_baselines: scan", zap.Error(err))
	}
	if err := s.SweepExpired(ctx); err != nil {
		s.logger.Error("sweep_expired: scan", zap.Error(err))
	}
	if err := s.RecordCloses(ctx); err != nil {
		s.logger.Error("record_closes: scan", zap.Error(err))
	}
	if err := s.TellCoordinators(ctx); err != nil {
		s.logger.Error("tell_coordinators: scan", zap.Error(err))
	}
	if s.idle != nil {
		if err := s.NoticeIdleResearchers(ctx); err != nil {
			s.logger.Error("notice_idle_researchers: scan", zap.Error(err))
		}
		if err := s.NoticeIdleCapacity(ctx); err != nil {
			s.logger.Error("notice_idle_capacity: scan", zap.Error(err))
		}
	}
}

// NoticeIdleResearchers tells every researcher due an idle notice that it is idle (agent.idle),
// every idleAfter while its stretch lasts, with what the platform knows at the notice. A platform
// experiment whose numbers cannot be read now is skipped, and its researchers are told at a
// later pass.
func (s *PlatformExperimentsService) NoticeIdleResearchers(ctx context.Context) error {
	due, err := s.idle.IdleResearchers(ctx, s.idleAfter)
	if err != nil {
		return err
	}
	byRun := map[string][]db.IdleResearcher{}
	for _, r := range due {
		byRun[r.PlatformExperimentID] = append(byRun[r.PlatformExperimentID], r)
	}
	for _, id := range slices.Sorted(maps.Keys(byRun)) {
		if err := s.noticeIdle(ctx, id, byRun[id]); err != nil {
			s.logger.Error("notice_idle_researchers: failed", zap.String("platform_experiment_id", id), zap.Error(err))
		}
	}
	return nil
}

// noticeIdle tells each researcher of due, of platform experiment id, that it is idle.
func (s *PlatformExperimentsService) noticeIdle(ctx context.Context, id string, due []db.IdleResearcher) error {
	pe, err := s.store.GetPlatformExperiment(ctx, id)
	if err != nil {
		return err
	}
	if pe == nil {
		return ErrPlatformExperimentNotFound
	}
	signups, err := s.store.ListAgentSignups(ctx, id)
	if err != nil {
		return err
	}
	rows, err := s.roster(ctx, id, signups, coordinatorReader)
	if err != nil {
		return err
	}
	capacity, err := s.runCapacity(ctx, pe)
	if err != nil {
		return err
	}
	quotas, err := s.ListQuotas(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, r := range due {
		notice := idleNotice{minutes: int(now.Sub(r.Since).Minutes()), fitsNow: capacity.FitsNow, minutesLeft: minutesUntil(pe.EndsAt, now)}
		for _, row := range rows {
			if row.AgentID == r.AgentID {
				notice.subagents, notice.turn = row.Subagents, row.Turn
			}
		}
		for _, q := range quotas {
			if q.AgentID == r.AgentID {
				notice.quota = q
			}
		}
		if err := s.idle.NoticeIdle(ctx, r, notice.detail()); err != nil {
			s.logger.Error("notice_idle_researchers: notice", zap.String("platform_experiment_id", id), zap.String("agent_id", r.AgentID), zap.Error(err))
		}
	}
	return nil
}

// idleNotice is what the platform knows of an idle researcher at its notice.
type idleNotice struct {
	minutes     int
	subagents   *int
	turn        *domain.AgentTurn
	fitsNow     *int
	minutesLeft *int
	quota       *AgentQuotaView
}

// detail is the notice as agent.idle carries it.
func (n idleNotice) detail() string {
	parts := []string{fmt.Sprintf("no job of its own ran on the platform for %d minutes, and it has none queued or running", n.minutes)}
	switch {
	case n.subagents != nil && n.turn != nil:
		parts = append(parts, fmt.Sprintf("its harness reports %d subagents running and its turn %s since %s", *n.subagents, n.turn.State, n.turn.Since.UTC().Format(time.RFC3339)))
	default:
		parts = append(parts, "its harness reports no state")
	}
	if n.fitsNow != nil {
		parts = append(parts, fmt.Sprintf("the free capacity places %d jobs shaped like the baseline job now", *n.fitsNow))
	}
	if n.minutesLeft != nil {
		parts = append(parts, fmt.Sprintf("%d minutes of the run are left", *n.minutesLeft))
	}
	if n.quota != nil {
		left := domain.RoundHours(max(0, n.quota.ShareAcceleratorHours-n.quota.UsedGuaranteedAccH-n.quota.UsedBurstAccH))
		parts = append(parts, fmt.Sprintf("%g of its share of %g accelerator hours are left", left, n.quota.ShareAcceleratorHours))
	}
	return strings.Join(parts, "; ")
}

// NoticeIdleCapacity tells whoever steers each running platform experiment with time left that
// its capacity holds places beyond its queued jobs (capacity.idle), every idleAfter while that
// lasts, whoever has jobs in flight and wherever the best stands against the target. A run whose
// numbers cannot be read is skipped, and told at a later pass.
func (s *PlatformExperimentsService) NoticeIdleCapacity(ctx context.Context) error {
	pes, err := s.store.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpRunning)
	if err != nil {
		return fmt.Errorf("list running platform experiments: %w", err)
	}
	for _, pe := range pes {
		if err := s.noticeIdleCapacity(ctx, pe); err != nil {
			s.logger.Error("notice_idle_capacity: failed", zap.String("platform_experiment_id", pe.ID), zap.Error(err))
		}
	}
	return nil
}

// noticeIdleCapacity records whether the capacity of pe holds places beyond its queued jobs, and
// tells it once that is due, with the numbers the situation reads.
func (s *PlatformExperimentsService) noticeIdleCapacity(ctx context.Context, pe *domain.PlatformExperiment) error {
	now := time.Now()
	notice := capacityNotice{minutesLeft: minutesUntil(pe.EndsAt, now)}
	if notice.minutesLeft != nil && *notice.minutesLeft > 0 {
		capacity, err := s.runCapacity(ctx, pe)
		if err != nil {
			return err
		}
		signups, err := s.store.ListAgentSignups(ctx, pe.ID)
		if err != nil {
			return err
		}
		rows, err := s.roster(ctx, pe.ID, signups, coordinatorReader)
		if err != nil {
			return err
		}
		notice.jobs = runJobsInFlight(rows)
		if capacity.FitsNow != nil {
			notice.free = *capacity.FitsNow - notice.jobs.Queued
		}
	}
	due, err := s.idle.MarkCapacityFree(ctx, pe.ID, notice.free > 0, s.idleAfter)
	if err != nil || due == nil {
		return err
	}
	objective, err := s.objective(ctx, pe, now)
	if err != nil {
		return err
	}
	if len(objective) > 0 {
		notice.primary = &objective[0]
	}
	notice.minutes = int(now.Sub(due.Since).Minutes())
	return s.idle.NoticeCapacityIdle(ctx, *due, notice.detail())
}

// capacityNotice is what the platform knows of a run whose capacity holds places free.
type capacityNotice struct {
	free        int
	minutes     int
	jobs        JobCounts
	minutesLeft *int
	primary     *SituationMetric
}

// detail is the notice as capacity.idle carries it. A target the best passed is said as passed,
// and the notice stands: the run goes on raising the best.
func (n capacityNotice) detail() string {
	parts := []string{fmt.Sprintf("the free capacity places %d more jobs shaped like the baseline job beyond the %d queued, and has for %d minutes", n.free, n.jobs.Queued, n.minutes),
		fmt.Sprintf("%d jobs of the run are queued and %d running", n.jobs.Queued, n.jobs.Running)}
	if n.minutesLeft != nil {
		parts = append(parts, fmt.Sprintf("%d minutes of the run are left", *n.minutesLeft))
	}
	switch m := n.primary; {
	case m == nil || m.BestValue == nil:
		parts = append(parts, "no best is measured yet")
	case m.GapToTargetPct == nil || m.Target == nil:
		parts = append(parts, fmt.Sprintf("the best %s is %g, against no target", m.Metric, *m.BestValue))
	case *m.GapToTargetPct > 0:
		parts = append(parts, fmt.Sprintf("the best %s is %g, %g%% short of the target %g", m.Metric, *m.BestValue, *m.GapToTargetPct, *m.Target))
	default:
		parts = append(parts, fmt.Sprintf("the best %s is %g, %g%% past the target %g", m.Metric, *m.BestValue, -*m.GapToTargetPct, *m.Target))
	}
	return strings.Join(parts, "; ")
}

// StartEndSweep runs SweepEnded on a ticker until ctx is cancelled: StartExpirySweep's interval
// would close a run up to that long after its end. Each pass is bounded by passTimeout.
func (s *PlatformExperimentsService) StartEndSweep(ctx context.Context, interval, passTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			passCtx, cancel := context.WithTimeout(ctx, passTimeout)
			if err := s.SweepEnded(passCtx); err != nil {
				s.logger.Error("sweep_ended: scan", zap.Error(err))
			}
			cancel()
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
