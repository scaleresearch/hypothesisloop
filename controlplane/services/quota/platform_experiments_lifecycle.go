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

	"github.com/google/uuid"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/api"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/db"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"go.uber.org/zap"
)

var ErrPlatformExperimentNotFound = errors.New("platform experiment not found")

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

// Create creates a new platform experiment in open status (accepting signups immediately).
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
	pe := &domain.PlatformExperiment{
		ID:                        "pe-" + uuid.New().String()[:8],
		ProjectID:                 req.ProjectID,
		Name:                      req.Name,
		Description:               req.Description,
		BudgetAcceleratorHours:    req.BudgetAcceleratorHours,
		MaxAgents:                 req.MaxAgents,
		Metrics:                   req.Metrics,
		ReportIntervalSeconds:     req.ReportIntervalSeconds,
		StartsAt:                  req.StartsAt,
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
		Repository:                req.Repository,
		AgentImage:                req.AgentImage,
		Actor:                     req.createdBy,
	}
	pe.BaselineDueAt = pe.OpenUntil(now, s.cfg.BaselineLimit)
	if req.prepare != nil {
		if err := req.prepare(ctx, pe); err != nil {
			return nil, err
		}
	}
	if err := s.store.CreatePlatformExperiment(ctx, pe); err != nil {
		return nil, fmt.Errorf("platform_experiments.Create: %w", err)
	}
	s.logger.Info("platform experiment created", zap.String("id", pe.ID), zap.String("name", pe.Name))
	return pe, nil
}

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
// held as patched to what that job did (checkBaseline), so its brief, its metrics and its budget
// cannot leave the baseline behind before it starts.
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
		patched.ReportIntervalSeconds = *patch.ReportIntervalSeconds
	}
	if patch.StartsAt != nil {
		patched.StartsAt = *patch.StartsAt
		patched.BaselineDueAt = patched.OpenUntil(time.Now().UTC(), s.cfg.BaselineLimit)
	}
	if patch.DurationHours != nil {
		patched.DurationHours = *patch.DurationHours
	}
	if patch.MaxConcurrentAccelerators != nil {
		patched.MaxConcurrentAccelerators = patch.MaxConcurrentAccelerators
	}
	if patch.BaselineJobID != nil {
		patched.BaselineJobID = *patch.BaselineJobID
	}
	checked, err := s.validate(CreatePlatformExperimentRequest{
		Name: patched.Name, Description: patched.Description,
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
		baseline, err := s.baseline(ctx, &patched, patched.BaselineJobID)
		if err != nil {
			return nil, fmt.Errorf("platform_experiments.Update: read baseline: %w", err)
		}
		if err := checkBaseline(&patched, baseline); err != nil {
			return nil, err
		}
		patched.JobEstimatedDurationHours = baselineHeadroom * baseline.ObservedHours
		if err := budgetCoversTrials(&patched, baseline.Job); err != nil {
			return nil, err
		}
	}
	pe = &patched
	if err := s.store.UpdatePlatformExperiment(ctx, pe, expectedStatus); err != nil {
		return nil, fmt.Errorf("platform_experiments.Update: %w", err)
	}
	return pe, nil
}

// maxAgentsReached refuses a researcher past max_agents, which counts researchers and nothing else.
func maxAgentsReached(limit int) error {
	return refuseSignup(409, "max_agents_reached", "max_agents is %d and that many researchers (agents of role competitor) have joined. One that finished or was cut still counts, and agents of role baseline, reviewers and the coordinator never do: the owner raises max_agents, or the agent joins with another role", limit)
}

func maxHostedReached(limit int) string {
	return fmt.Sprintf("the platform runs at most %d machines for one platform experiment at once. One is kept for an agent of role baseline, which measures the baseline nothing starts without, and its coordinator and its other hosted agents take the rest. An agent that finished holds none", limit)
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
	return api.Refuse(http.StatusConflict, "no_researchers", fmt.Sprintf("platform experiment %s has its baseline and no researcher to share its budget: add one with `hl platform-experiments agents add --hosted` (POST /platform-experiments/%s/agents), and the platform starts the run", id, id))
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

const (
	addBaselineAgent = "`hl platform-experiments agents add --hosted --role baseline`"
	declareBaseline  = "`hl platform-experiments update --baseline-job JOB --description-file brief.md`, the brief's BASELINE block naming JOB and its code_ref"
)

// waiting is why the open pe, whose roster is signups, is not running yet: the reason and the
// sentence its status_reason and its roster answer with.
func (s *PlatformExperimentsService) waiting(ctx context.Context, pe *domain.PlatformExperiment, signups []domain.AgentSignup) (*domain.StatusReason, error) {
	_, err := s.notStartable(ctx, pe, signups, time.Now().UTC())
	var refused *api.Refusal
	switch {
	case errors.As(err, &refused):
		return &domain.StatusReason{Reason: refused.Reason, Message: refused.Message}, nil
	case err != nil:
		return nil, err
	}
	return &domain.StatusReason{Reason: "starting", Message: "the baseline is declared and the platform starts the run within a minute"}, nil
}

// nextStep is what reader does about an open pe that waits for reason, a sentence that ends in
// a space, or nothing when all it can do is wait. A coordinator told only that a baseline was
// required waited for one and added no agent to measure it, so the step names its command.
func nextStep(pe *domain.PlatformExperiment, reason string, signups []domain.AgentSignup, reader auth.Principal) string {
	if reader.Kind == auth.KindAgent {
		measures := slices.ContainsFunc(signups, func(signup domain.AgentSignup) bool {
			return signup.AgentID == reader.AgentID && signup.Role == domain.SignupRoleBaseline
		})
		switch {
		case !measures:
			return "Every job you submit is refused until the platform starts the run: prepare, then end your turn, and the start arrives as an update. "
		case reason == "baseline_required":
			return "Your job is admitted now: measure the baseline with one job (hl job submit), file its summary (hl job summary), then hl platform-experiments agents finish. "
		}
		return ""
	}
	if reason != "baseline_required" {
		return ""
	}
	if reader.Kind != auth.KindCoordinator && pe.CoordinatorHosted {
		return "Its coordinator adds the agent of role baseline that measures it and declares the job: nothing is yours to do. "
	}
	var baselines, measuring []string
	for _, signup := range signups {
		if signup.Role != domain.SignupRoleBaseline {
			continue
		}
		baselines = append(baselines, signup.Name)
		if signup.FinishedAt == nil && (signup.Instance == nil || !signup.Instance.Stopped) {
			measuring = append(measuring, signup.Name)
		}
	}
	switch {
	case len(baselines) == 0:
		return "Add a baseline agent with " + addBaselineAgent + ": none has joined. Once its job is COMPLETED with a summary, declare it with " + declareBaseline + ". "
	case len(measuring) == 0:
		return "Every baseline agent (" + strings.Join(baselines, ", ") + ") finished or stopped: declare the COMPLETED job one of them left (hl job list) with " + declareBaseline + ". When none left a job that can be declared, add another baseline agent with " + addBaselineAgent + ". "
	}
	return "Baseline agent " + strings.Join(measuring, ", ") + " is measuring: once its job is COMPLETED with a summary, declare it with " + declareBaseline + ". "
}

// Describe sets on a draft where it stands with its planner, and on an open pe why it is not
// running yet and what reader does about it (StatusReason). It is the one place that says so.
func (s *PlatformExperimentsService) Describe(ctx context.Context, pe *domain.PlatformExperiment, reader auth.Principal) error {
	if pe.Status == domain.PlatformExpDraft {
		pe.StatusReason = draftReason(pe)
		return nil
	}
	if pe.Status != domain.PlatformExpOpen {
		return nil
	}
	signups, err := s.store.ListAgentSignups(ctx, pe.ID)
	if err != nil {
		return fmt.Errorf("platform_experiments.Describe: %w", err)
	}
	why, err := s.waiting(ctx, pe, signups)
	if err != nil {
		return err
	}
	why.Message = nextStep(pe, why.Reason, signups, reader) + why.Message
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
	started, quotas, err := s.store.StartPlatformExperimentTx(ctx, pe.ID, func(participants []db.StartParticipant) ([]*domain.AgentQuota, []domain.Stage, error) {
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
		exploreFrac := stages[0].LengthPct / 100.0
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
			allocated = append(allocated, &domain.AgentQuota{
				ID:                         uuid.New().String(),
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

// SweepExpired closes every platform experiment whose ClosesAt has passed: a run at its end, a
// draft whose planning limit passed or whose answer no person took up, which releases its
// machine, and an open one that is not running by its BaselineDueAt: the machines that were to
// measure its baseline would be rented for nothing.
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
// declared, its starts_at is absent or past, and it has a researcher and budget left for one. It
// is the one way a run starts. One that is not startable waits, saying why in its status_reason
// (Describe), until SweepExpired closes it.
func (s *PlatformExperimentsService) SweepAutoStart(ctx context.Context) error {
	now := time.Now().UTC()
	pes, err := s.store.ListPlatformExperimentsByStatus(ctx, domain.PlatformExpOpen)
	if err != nil {
		return fmt.Errorf("list open platform experiments: %w", err)
	}
	for _, pe := range pes {
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

// StartExpirySweep runs SweepAutoStart and SweepExpired on a ticker until ctx is cancelled.
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
		}
	}
}
