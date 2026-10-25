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

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

var ErrPlatformExperimentNotFound = errors.New("platform experiment not found")

// coordinatorReader reads a roster for the run's own bookkeeping, where no message allowance is shown.
var coordinatorReader = domain.Actor{Kind: domain.ActorCoordinator}

// checkLength holds a platform experiment to a run of at most quota.max_platform_experiment_days.
// Until its end it may rent machines.
func (s *Service) checkLength(durationHours float64) error {
	if length := time.Duration(durationHours * float64(time.Hour)); length > s.cfg.MaxRunLength {
		return refuseUpdate(422, "run_too_long", "it would run %.1f days, and a platform experiment runs at most %g days (quota.max_platform_experiment_days): state a shorter duration_hours",
			length.Hours()/24, s.cfg.MaxRunLength.Hours()/24)
	}
	return nil
}

// withSettings returns spec with the platform's settings in the fields a request may leave to
// them: scheduler.default_report_interval_seconds, max_agents as the executors of agents, stages.default as a run of max_agents
// executors has it (domain.RunStages) and quota.default_quota_tier.
func (s *Service) withSettings(spec experimentspec.Spec) experimentspec.Spec {
	if spec.ReportIntervalSeconds == 0 {
		spec.ReportIntervalSeconds = s.cfg.DefaultReportIntervalSeconds
	}
	if spec.MaxAgents == 0 {
		spec.MaxAgents = spec.Executors()
	}
	if len(spec.Stages) == 0 {
		spec.Stages = domain.RunStages(s.cfg.DefaultStages, spec.MaxAgents)
	}
	if spec.DefaultQuotaTier == "" {
		spec.DefaultQuotaTier = s.cfg.DefaultQuotaTier
	}
	return spec
}

// specReasons is the refusal reason of a problem of the spec, by its key: invalid_experiment for
// a key it does not name.
var specReasons = map[string]string{
	"metrics":                  "invalid_metrics",
	"stages":                   "invalid_stages",
	"budget_accelerator_hours": "invalid_cost",
	"max_agents":               "invalid_agents",
	"agents":                   "invalid_agents",
	"duration_hours":           "invalid_length",
	"agent_image":              "invalid_agent_image",
}

// specProblems is every rule spec breaks (experimentspec.Spec.Validate), each a refusal on its key.
func specProblems(spec experimentspec.Spec) refusal.Problems {
	var problems refusal.Problems
	var broken experimentspec.Problems
	if !errors.As(spec.Validate(), &broken) {
		return nil
	}
	for _, problem := range broken {
		reason, ok := specReasons[problem.Key]
		if !ok {
			reason = "invalid_experiment"
		}
		refused := refusal.Refuse(http.StatusUnprocessableEntity, reason, problem.Message)
		if problem.Key != "" {
			refused.On(problem.Key)
		}
		problems = append(problems, refused)
	}
	return problems
}

// CheckCreate refuses a create body for every field it leaves out or states wrong that the body
// alone shows, all in one answer, before anything is read for it.
func CheckCreate(req CreatePlatformExperimentRequest) error {
	var problems refusal.Problems
	if strings.TrimSpace(req.ProjectID) == "" {
		problems = append(problems, refusal.Refuse(http.StatusUnprocessableEntity, refusal.ReasonInvalidRequest, "project_id is required: the project the experiment belongs to").On("project_id"))
	}
	return append(problems, specProblems(req.Spec)...).Err()
}

// validate is the one check of what a platform experiment is, whoever states it: a create, a
// planner's proposal or a patch of an existing one. The spec's own rules
// come first (experimentspec.Spec.Validate), then those of the platform's settings. Its agents are
// checked against the catalog where they are resolved (resolvedPool, proposedPool). It
// returns req as it is stored, trimmed and with the platform's settings where it leaves a field to
// them, and refuses with every problem it finds.
func (s *Service) validate(req CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	problems := specProblems(req.Spec)
	req.Spec = s.withSettings(req.Spec.Trimmed())
	if err := problems.Add(s.checkLength(req.DurationHours)); err != nil {
		return req, err
	}
	if b := req.ModelBudgetUSD; b != nil && (*b <= 0 || *b > s.cfg.MaxModelBudgetUSD) {
		problems = append(problems, refusal.Refuse(422, domain.ReasonModelBudgetTooHigh, fmt.Sprintf("model_budget_usd %g is not above 0 and at most %g (quota.max_model_budget_usd): leave it out for the platform's default of %g", *b, s.cfg.MaxModelBudgetUSD, s.cfg.DefaultModelBudgetUSD)).On("model_budget_usd"))
	}
	return req, problems.Err()
}

// requireReachable refuses a platform experiment coming into being where nothing could run it:
// a create in an org whose jobs no cluster takes now, and a proposal or an accept sized for a
// type none of them has. sizedFor is nil for a create, which states no type. One that exists is
// never held to it, since a cluster that reports nothing for a moment must not stop a
// coordinator declaring its baseline or an owner renaming a run. It answers the clusters that
// have the type it was sized for.
func (s *Service) requireReachable(ctx context.Context, projectID string, sizedFor *domain.AcceleratorType) ([]domain.Cluster, error) {
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

// Create creates a new platform experiment in open status (accepting signups immediately), with
// the agents req carries signed up in the same write.
func (s *Service) Create(ctx context.Context, req CreatePlatformExperimentRequest) (*domain.PlatformExperiment, error) {
	now := time.Now().UTC()
	req, err := s.checked(ctx, req)
	if err != nil {
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
		Agents:                    req.Agents,
		Metrics:                   req.Metrics,
		ReportIntervalSeconds:     req.ReportIntervalSeconds,
		StartsAt:                  domain.ClockStart(req.StartsAt, now),
		DurationHours:             req.DurationHours,
		Status:                    domain.PlatformExpOpen,
		Stages:                    req.Stages,
		CurrentStage:              1,
		DefaultQuotaTier:          req.DefaultQuotaTier,
		MaxConcurrentAccelerators: req.MaxConcurrentAccelerators,
		ModelBudgetUSD:            req.ModelBudgetUSD,
		CreatedAt:                 now,
		UpdatedAt:                 now,
		CoordinatorInAgentSandbox: req.CoordinatorInAgentSandbox,
		Coordinator:               req.coordinator,
		MaxAgentSandboxes:         req.maxAgentSandboxes,
		Repository:                req.repository.Named,
		RepositoryURL:             req.repository.URL,
		Branch:                    req.Branch,
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

// checked is req as Create stores it, refused for every rule of the platform experiment it
// states and of the platform's settings and clusters.
func (s *Service) checked(ctx context.Context, req CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	req, err := s.validate(req)
	if err != nil {
		return req, err
	}
	if _, err := s.requireReachable(ctx, req.ProjectID, nil); err != nil {
		return req, err
	}
	if req.CoordinatorInAgentSandbox != (req.coordinator != nil) {
		return req, fmt.Errorf("platform_experiments.Create: coordinator_in_agentsandbox is %v and its model is resolved: %v", req.CoordinatorInAgentSandbox, req.coordinator != nil)
	}
	if err := s.requireContinuable(ctx, req.ProjectID, req.repository.Named, req.Continues); err != nil {
		return req, err
	}
	return req, s.requireContinuedMetrics(ctx, req.Continues, req.Metrics)
}

// requireContinuable refuses a run to continue that is not one: continues names a closed platform
// experiment of the project that keeps its code in repository and declared a baseline, whose
// commit the new baseline starts from. Empty continues starts fresh.
func (s *Service) requireContinuable(ctx context.Context, projectID, repository, continues string) error {
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
func (s *Service) requireContinuedMetrics(ctx context.Context, continues string, metrics []domain.MetricDefinition) error {
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
func (s *Service) stored(ctx context.Context, id string) (*domain.PlatformExperiment, error) {
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
	return refusal.Refuse(http.StatusNotFound, "not_found", "no platform experiment "+id)
}

// CreateExperiment creates the platform experiment req asks for, for by: in the repository it
// names, created from the experiment template when it does not exist, or one the platform
// creates, and with the sandboxed agents its pool names.
func (s *Service) CreateExperiment(ctx context.Context, req CreatePlatformExperimentRequest, by domain.Actor) (*domain.PlatformExperiment, error) {
	named, err := s.repositoryOf(ctx, req.ProjectID, req.Repository, req.Branch, req.RepositoryAccount)
	if err != nil {
		return nil, err
	}
	req.Repository = named
	create, err := s.prepared(ctx, req, by)
	if err != nil {
		return nil, err
	}
	if create.repository, err = s.chooseRepository(ctx, req.ProjectID, create.Repository, req.RepositoryAccount); err != nil {
		return nil, err
	}
	pe, err := s.Create(ctx, create)
	if err != nil {
		return nil, s.refusal("create platform experiment", err)
	}
	return pe, nil
}

// CheckExperiment refuses req, for by, as CreateExperiment would, and creates nothing. It checks
// everything but its repository and branch, which POST /projects/{id}/repositories and a push of
// the branch settle: a client that pushes the branch checks the rest first.
func (s *Service) CheckExperiment(ctx context.Context, req CreatePlatformExperimentRequest, by domain.Actor) error {
	create, err := s.prepared(ctx, req, by)
	if err != nil {
		return err
	}
	create.repository = RepositoryChoice{Named: req.Repository}
	if _, err := s.checked(ctx, create); err != nil {
		return s.refusal("check platform experiment", err)
	}
	return nil
}

// prepared is req with its agents resolved and checked, its coordinator resolved and checked, and
// the agents it is created with, for by.
func (s *Service) prepared(ctx context.Context, req CreatePlatformExperimentRequest, by domain.Actor) (CreatePlatformExperimentRequest, error) {
	catalog, err := s.store.ProjectAgentTypeCatalog(ctx, req.ProjectID)
	if err != nil {
		return req, auth.Internal(s.logger, "create platform experiment", err)
	}
	if req.Agents, err = s.resolvedPool(ctx, catalog, req.Agents); err != nil {
		return req, err
	}
	create, err := withAgentSandbox(req, s.launch)
	if err != nil {
		return req, s.refusal("create platform experiment", err)
	}
	if err := s.served.CheckChoice(ctx, "coordinator_model", create.CoordinatorModel); err != nil {
		return req, s.refusal("create platform experiment", err)
	}
	create.createdBy = &by
	if create.CoordinatorInAgentSandbox {
		if create.agents, create.signups, err = sandboxedAgents(catalog, poolAgents(create.Agents), by, time.Now().UTC()); err != nil {
			return req, s.refusal("create platform experiment", err)
		}
	}
	return create, nil
}
