package platformexperiments

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
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

// CreateExperiment creates the platform experiment req asks for, for by: in the repository it
// names or one the platform creates, with what the experiment file states under req when it asks
// from_repo, and with the hosted agents its pool names.
func (s *PlatformExperimentsService) CreateExperiment(ctx context.Context, req CreatePlatformExperimentRequest, by domain.Actor) (*domain.PlatformExperiment, error) {
	named, err := s.repositoryOf(ctx, req.ProjectID, req.Repository, req.RepositoryAccount)
	if err != nil {
		return nil, err
	}
	req.Repository = named
	// Never the experiment file's to state, so it is held before the repository is read.
	if err := s.checkLength(req.DurationHours); err != nil {
		return nil, err
	}
	if err := domain.ValidateAgentImage(req.AgentImage); err != nil {
		return nil, refusal.Refuse(http.StatusUnprocessableEntity, "invalid_agent_image", err.Error())
	}
	if req.FromRepo {
		if req.Repository == "" {
			return nil, refusal.Refuse(http.StatusUnprocessableEntity, "no_repository", "from_repo reads "+domain.ExperimentFile+" of the repository the request names, and it names none: set repository")
		}
		named := req.Repository
		body, err := s.fromRepo(ctx, req)
		body.Repository = named
		if err != nil {
			return nil, err
		}
		req = body
	}
	catalog, err := s.store.ProjectAgentTypeCatalog(ctx, req.ProjectID)
	if err != nil {
		return nil, auth.Internal(s.logger, "create platform experiment", err)
	}
	if req.Researchers, err = s.resolvedPool(catalog, req.Researchers); err != nil {
		return nil, err
	}
	hosting, err := withHosting(req, s.launch)
	if err != nil {
		return nil, s.refusal("create platform experiment", err)
	}
	hosting.createdBy = &by
	if hosting.CoordinatorHosted {
		if hosting.agents, hosting.signups, err = hostedAgents(catalog, poolAgents(hosting.Researchers), by, time.Now().UTC()); err != nil {
			return nil, s.refusal("create platform experiment", err)
		}
	}
	if hosting.repository, err = s.chooseRepository(ctx, req.ProjectID, hosting.Repository, req.RepositoryAccount); err != nil {
		return nil, err
	}
	pe, err := s.Create(ctx, hosting)
	// What is wrong with a body laid over the experiment file may be the file's doing.
	var refused *refusal.Refusal
	if req.FromRepo && errors.As(err, &refused) {
		return nil, refusal.Refuse(refused.GetStatus(), refused.Reason, "with "+domain.ExperimentFile+" of the repository under the request: "+refused.Message)
	}
	var answered interface{ GetStatus() int }
	if errors.As(err, &answered) {
		return nil, err
	}
	if err != nil {
		return nil, auth.Internal(s.logger, "create platform experiment", err)
	}
	return pe, nil
}
