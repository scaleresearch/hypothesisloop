package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"time"
)

// PlatformExperimentStatus is the lifecycle state of a platform experiment.
type PlatformExperimentStatus string

const (
	// PlatformExpDraft is a platform experiment being planned from a goal: a planner the platform
	// runs works out what to measure and proposes the rest, nobody else joins, and no job runs
	// until a person accepts the proposal, which opens it.
	PlatformExpDraft   PlatformExperimentStatus = "draft"
	PlatformExpOpen    PlatformExperimentStatus = "open"
	PlatformExpRunning PlatformExperimentStatus = "running"
	PlatformExpClosed  PlatformExperimentStatus = "closed"
)

// ValidPlatformExperimentStatus reports whether s is one of the recognized statuses. As with
// ValidExperimentStatus, filtering on an unrecognized value would otherwise reach Postgres as a
// bad enum literal and surface as a 500.
func ValidPlatformExperimentStatus(s PlatformExperimentStatus) bool {
	switch s {
	case PlatformExpDraft, PlatformExpOpen, PlatformExpRunning, PlatformExpClosed:
		return true
	default:
		return false
	}
}

// MetricRole classifies what a declared MetricDefinition is for. Declaring a metric no longer
// implies it's ranked — see MetricDefinition.Role.
type MetricRole string

const (
	// MetricRoleRanking counts for stage cuts and standings — today's behaviour, and the
	// default when Role is left empty, so an experiment created before this field existed
	// keeps ranking on every declared metric exactly as it always did.
	MetricRoleRanking MetricRole = "ranking"
	// MetricRoleConstraint must be reported and must satisfy Bound. It is judged per job: a
	// completed job that violates it or never reported it ranks nowhere (metricsdb.RankableJobs),
	// and the metric itself is never ranked or cut on.
	MetricRoleConstraint MetricRole = "constraint"
	// MetricRoleAttribute is reported and shown, never ranked or cut on, and never gates
	// standings eligibility — "which kind of result is this" facts (precision class, result
	// category, ...) that the objective may care about but that have no single order.
	MetricRoleAttribute MetricRole = "attribute"
	// MetricRoleReference is an outside system the goal compares against, measured under a key
	// of its own: reported and shown beside the standings with the machine it ran on, never
	// ranked, cut on or gating, and not required of the baseline, which says why it left one out.
	MetricRoleReference MetricRole = "reference"
)

// ValidMetricRole reports whether role is one of the declared roles, or empty (which
// PlatformExperiment.Metrics' consumers treat as MetricRoleRanking for backward compatibility).
func ValidMetricRole(role MetricRole) bool {
	switch role {
	case "", MetricRoleRanking, MetricRoleConstraint, MetricRoleAttribute, MetricRoleReference:
		return true
	default:
		return false
	}
}

// EffectiveRole returns d.Role, defaulting empty to MetricRoleRanking.
func (d MetricDefinition) EffectiveRole() MetricRole {
	if d.Role == "" {
		return MetricRoleRanking
	}
	return d.Role
}

// MetricDefinition declares a single metric that jobs in a PlatformExperiment must emit.
// The key is the Prometheus metric name; direction is "maximize" or "minimize". Role decides
// what the declaration is *for* — only MetricRoleRanking metrics ever rank or cut (see
// ValidateMetricDefinitions); Bound is required and only meaningful for MetricRoleConstraint,
// expressed in the same direction as Direction (maximize: value must be >= Bound; minimize:
// value must be <= Bound). TargetVsBaseline, optional and only on a ranking metric, is the goal
// as a signed change against the baseline's value, in fractions of its magnitude. Deterministic
// declares that the same code and data give the same value on any machine and in every run.
type MetricDefinition struct {
	Key              string     `json:"key"`
	Direction        string     `json:"direction"` // "maximize" | "minimize"
	Role             MetricRole `json:"role,omitempty"`
	Bound            *float64   `json:"bound,omitempty"`
	TargetVsBaseline *float64   `json:"target_vs_baseline,omitempty" doc:"Optional, on a ranking metric: the goal as a change against the declared baseline's value, in fractions of its magnitude. 0.5 is 50% above the baseline, -0.3 is 30% below it. Positive for a metric to maximize, negative for one to minimize."`
	Deterministic    bool       `json:"deterministic,omitempty" doc:"Optional: the same code and data give the same value on any machine and in every run, as an exact count or check does and a timing never does. Its values compare across machines and need no repeat."`
}

// AllRankingDeterministic reports whether defs declare a ranking metric and every one of them is
// deterministic: then a job's result needs neither a repeat nor a control on its own machine.
func AllRankingDeterministic(defs []MetricDefinition) bool {
	ranking := false
	for _, d := range defs {
		if d.EffectiveRole() != MetricRoleRanking {
			continue
		}
		if !d.Deterministic {
			return false
		}
		ranking = true
	}
	return ranking
}

// Target is the value TargetVsBaseline sets against baseline: nil without one, or with a baseline
// of 0, which no fraction of its magnitude moves.
func (d MetricDefinition) Target(baseline float64) *float64 {
	if d.TargetVsBaseline == nil || baseline == 0 {
		return nil
	}
	target := baseline + *d.TargetVsBaseline*math.Abs(baseline)
	return &target
}

// Holds reports whether value satisfies the bound of a constraint metric.
func (d MetricDefinition) Holds(value float64) bool {
	if d.Direction == "minimize" {
		return value <= *d.Bound
	}
	return value >= *d.Bound
}

// Better reports whether a is strictly better than b in the metric's direction.
func (d MetricDefinition) Better(a, b float64) bool {
	if d.Direction == "minimize" {
		return a < b
	}
	return a > b
}

// ValidateMetricDefinitions enforces the metric contract at creation, so nothing downstream
// (stage cuts, standings) needs to defend against a malformed one. Fail-fast per important.md #1.
func ValidateMetricDefinitions(defs []MetricDefinition) error {
	seen := make(map[string]bool, len(defs))
	for i, d := range defs {
		if d.Key == "" {
			return fmt.Errorf("metric %d: key is required", i)
		}
		if seen[d.Key] {
			return fmt.Errorf("metric %d: duplicate key %q", i, d.Key)
		}
		seen[d.Key] = true
		if d.Direction != "maximize" && d.Direction != "minimize" {
			return fmt.Errorf("metric %q: direction must be \"maximize\" or \"minimize\", got %q", d.Key, d.Direction)
		}
		if !ValidMetricRole(d.Role) {
			return fmt.Errorf("metric %q: role must be one of ranking, constraint, attribute, reference (or empty), got %q", d.Key, d.Role)
		}
		if d.EffectiveRole() == MetricRoleConstraint && d.Bound == nil {
			return fmt.Errorf("metric %q: constraint role requires bound", d.Key)
		}
		if d.EffectiveRole() != MetricRoleConstraint && d.Bound != nil {
			return fmt.Errorf("metric %q: bound is only meaningful for constraint role", d.Key)
		}
		if err := d.validateTarget(); err != nil {
			return err
		}
	}
	return nil
}

// validateTarget refuses a target that is not a ranking metric's or lies on the wrong side of the
// baseline for its direction, which the run would have reached before it started.
func (d MetricDefinition) validateTarget() error {
	t := d.TargetVsBaseline
	switch {
	case t == nil:
		return nil
	case d.EffectiveRole() != MetricRoleRanking:
		return fmt.Errorf("metric %q: target_vs_baseline is only meaningful for ranking role", d.Key)
	case math.IsNaN(*t) || math.IsInf(*t, 0):
		return fmt.Errorf("metric %q: target_vs_baseline must be a finite number", d.Key)
	case d.Direction == "maximize" && *t <= 0:
		return fmt.Errorf("metric %q: target_vs_baseline %g is not above the baseline, and the metric is maximized: state a positive change, 0.5 for 50%% above it", d.Key, *t)
	case d.Direction == "minimize" && *t >= 0:
		return fmt.Errorf("metric %q: target_vs_baseline %g is not below the baseline, and the metric is minimized: state a negative change, -0.3 for 30%% below it", d.Key, *t)
	}
	return nil
}

// PrimaryRankingMetric is the first declared ranking metric, the primary one by convention.
func PrimaryRankingMetric(defs []MetricDefinition) (MetricDefinition, bool) {
	for _, d := range defs {
		if d.EffectiveRole() == MetricRoleRanking {
			return d, true
		}
	}
	return MetricDefinition{}, false
}

// RequireRankingMetric rejects a metric contract that can never rank anyone. Constraint and
// attribute metrics gate and describe; only a ranking metric orders a field. Without one the
// ladder runs to the end cutting nobody and the results endpoint returns no standings — a run
// that consumed a real budget and answered nothing, silently. Enforced where the contract is
// written (create/update), not on read: a row predating this rule must still reconcile.
func RequireRankingMetric(defs []MetricDefinition) error {
	if _, ok := PrimaryRankingMetric(defs); ok {
		return nil
	}
	return fmt.Errorf("at least one metric must have role \"ranking\": nothing else can order the field or cut a stage")
}

// MetricDefinitionsEqual reports whether two metric declarations are the same set in the same
// order — order matters (the first ranking metric is primary), so this is not a set comparison.
func MetricDefinitionsEqual(a, b []MetricDefinition) bool {
	return reflect.DeepEqual(a, b)
}

// MaxAgentImageLength bounds an image reference.
const MaxAgentImageLength = 512

// ValidateAgentImage accepts an image reference a launcher can be handed: printable, without
// spaces. Whether the image exists is the launcher's provider's to say.
func ValidateAgentImage(image string) error {
	if len(image) > MaxAgentImageLength {
		return fmt.Errorf("agent_image is at most %d characters", MaxAgentImageLength)
	}
	for _, r := range image {
		if r <= ' ' || r > '~' {
			return fmt.Errorf("agent_image must be an image reference: printable characters with no spaces")
		}
	}
	return nil
}

// ExperimentFile is where a platform experiment's repository keeps the base of its definition,
// read at the head of its default branch when one is created with from_repo.
const ExperimentFile = ".hypothesisloop/experiment.yaml"

// MaxProseBytes caps the description and the summary: every read of the platform experiment
// answers both whole, and every agent reads it often.
const MaxProseBytes = 32 << 10

// PlatformExperiment is the operator-defined compute envelope agents compete within.
type PlatformExperiment struct {
	ID                     string  `json:"id"`
	ProjectID              string  `json:"project_id"`
	Name                   string  `json:"name"`
	Description            string  `json:"description"`
	BudgetAcceleratorHours float64 `json:"budget_accelerator_hours"` // total compute in accelerator-hours (AccH), H100-equivalent
	// ConsumedAcceleratorHours is what the platform experiment has spent of its budget: what its
	// jobs that ended were billed, measuring the baseline included. Read from the metrics store
	// when one platform experiment is read, stored nowhere, and left out of a list.
	ConsumedAcceleratorHours *float64 `json:"consumed_accelerator_hours,omitempty" doc:"What it has spent of budget_accelerator_hours: what its jobs that ended were billed, the baseline's among them. A running job's cost joins when the job ends. On a read of one platform experiment that is no draft, and the whole spend once it is closed."`
	MaxAgents                int      `json:"max_agents" doc:"How many researchers may be at work at once, the one count of them: the agents of role competitor, whatever their agent type, that have not finished, were not cut, were not replaced and still have a machine. One that ended leaves its place to another. The coordinator, a draft's planner and agents of role baseline, which measure the baseline before the run and are not ranked, never count, nor do reviewers. Its owner changes it, never below the researchers at work nor below the total of researchers. On a draft: the most researchers its planner may propose, which accepting replaces with the number accepted."`
	// Researchers is the desired state of who researches: how many of each agent type and model
	// are to be at work, at most MaxAgents in all. The platform keeps it while the run goes on
	// (quota.KeepResearchers). Optional; empty on a draft, which its accepted proposal fills.
	Researchers ResearcherPool `json:"researchers" doc:"The researchers it is to have at work, an entry per agent type and model with its count, at most max_agents in all. Optional. Where the platform hosts the coordinator they are agents the platform runs: it creates them with the platform experiment, or with the accepted proposal, and while the run goes on it adds a new researcher of the same agent type, model and hyperparameters in the place of one that finished or whose machine stopped with no restart left, which agent.replaced tells, until less of the run is left than one trial of its baseline job (job_estimated_duration_hours, at least 15 minutes) or the budget its researchers left unspent holds no trial for a newcomer. A stage boundary lowers the count of the entry of each researcher it cuts, so nobody takes a cut researcher's place. The coordinator may add researchers beyond them, up to max_agents. Elsewhere they say who is wanted and nothing is added by the platform."`
	// ResearchersAtWork counts who is at work against the pool, on every answer about one platform
	// experiment (quota.PlatformExperimentsHandler.Answer), stored nowhere.
	ResearchersHeld   string             `json:"researchers_held,omitempty" doc:"On a running platform experiment whose platform hosts the coordinator, the places of researchers it keeps empty and why, absent when none: a researcher that ended before submitting a job or within 10 minutes of joining is never replaced, and one whose pool entry had 3 replacements in the last hour waits. platform_experiment.status tells each change. On a read of one."`
	ResearchersAtWork *ResearchersAtWork `json:"researchers_at_work,omitempty" doc:"The researchers at work, counted by the rule max_agents holds to: in all, for each entry of researchers, and outside_pool for those of an agent type and model it names no entry of. Absent on a list."`
	// MaxConcurrentAccelerators bounds accelerators-in-flight (SUBMITTED+RUNNING, summed across
	// this platform experiment's agents' jobs) at any moment. Nil defers to
	// quota.default_max_concurrent_accelerators. Speculative submission makes capacity appear on
	// demand and removes the live-capacity ceiling that used to bound concurrency implicitly —
	// this is the explicit replacement, enforced in ReserveAdmittedFlavorTx on every submit.
	MaxConcurrentAccelerators *int               `json:"max_concurrent_accelerators,omitempty"`
	Metrics                   []MetricDefinition `json:"metrics"`                 // metrics jobs must emit; used for ranking
	ReportIntervalSeconds     int                `json:"report_interval_seconds"` // expected reporting cadence (for silent-eviction guard)
	// StartsAt is when the clock that runs started. While it is open that is the moment it was
	// created or accepted, or a later starts_at stated, and its baseline is due BaselineDueAt from
	// then. Once it runs it is the moment the run started, when its baseline was declared and
	// nothing else was missing, and DurationHours count from it: EndsAt is the two added.
	// StartedAt is the moment a researcher was first at work in the running platform experiment,
	// which is the start itself when one of them is run by its owner and otherwise the moment the
	// first machine the platform runs for one was reported running.
	StartsAt      time.Time `json:"starts_at" doc:"While it is open: when its baseline began to be measured, the moment it was created or accepted, or a later starts_at stated. Once it runs: the moment the run started, once its baseline was declared, from which duration_hours count. The zero time on a draft."`
	StartedAt     time.Time `json:"started_at,omitzero" doc:"The moment a researcher was first at work in the running platform experiment. That is the moment it became running when a researcher is run by its owner, and otherwise the moment the first researcher's machine was reported running. Absent before, and on a running one that still waits for that machine (status_reason agents_starting)."`
	DurationHours float64   `json:"duration_hours,omitzero" doc:"How long the run lasts, the research time, counted from the moment it starts, once its baseline is declared. Measuring the baseline takes nothing from it: baseline_due_at bounds that. Absent on a draft."`
	// A platform experiment has exactly one stored moment at which the platform closes it if
	// nobody does, under the name of what ends then: PlanningEndsAt or ProposalExpiresAt on a
	// draft, BaselineDueAt while it is open, AgentsDueAt while it runs and no researcher is at
	// work, EndsAt from then. EndsAt of one that waits for a researcher is StartsAt plus
	// DurationHours. One that closed keeps the one it had. See ClosesAt.
	EndsAt            time.Time `json:"ends_at,omitzero" doc:"When the run ends and the platform closes it: starts_at, the start of the run, plus duration_hours. Absent until the run starts."`
	AgentsDueAt       time.Time `json:"agents_due_at,omitzero" doc:"On a running platform experiment with no started_at: when the platform closes it, with close_reason agents_not_started, if no researcher's machine is running by then. Never later than ends_at."`
	BaselineDueAt     time.Time `json:"baseline_due_at,omitzero" doc:"On an open platform experiment: when the platform closes it, with close_reason baseline_limit_reached, if it is not running by then. starts_at plus the minutes its baseline was given: the ones accepted for a draft, at most planning.baseline_limit_minutes, and that limit for one created whole. It moves out by the time a job of the baseline waits on the platform, for its environment to be built or for its cluster to start it, at most the 90 minutes the platform spends on one build, so that time is not the baseline agent's."`
	PlanningEndsAt    time.Time `json:"planning_ends_at,omitzero" doc:"On a draft being planned or revised: when the platform closes it if its planner has answered nothing by then."`
	ProposalExpiresAt time.Time `json:"proposal_expires_at,omitzero" doc:"On a draft whose planner proposed or declined: when the platform closes it if no person has accepted or revised it by then."`
	// Activity is what a draft's planner last said it is doing.
	Activity *AgentActivity `json:"activity,omitempty" doc:"On a draft whose planner's machine is up: what the planner is doing now and since when, as on each agent of GET /platform-experiments/{id}/agents. Absent on anything but a draft, where each agent's is in the roster and the coordinator's on coordinator."`
	// PlannerEndsAt is until when the machine of an answered draft's planner is kept.
	PlannerEndsAt *time.Time               `json:"planner_ends_at,omitempty" doc:"On a draft whose planner proposed or declined: until when the planner's machine is kept, the answer plus the platform's planning.planner_kept_minutes and never past the planning limit. A revision (POST /platform-experiments/{id}/revise) asked for before it reaches the planner that is still there, as its next update, and is answered with nothing to boot. One asked for later starts the planner again with its session, as before. Absent while the planner works."`
	Status        PlatformExperimentStatus `json:"status"`
	// StatusReason is why the platform experiment is in its status, in every status. Worked out
	// when one is read and stored nowhere.
	StatusReason *StatusReason `json:"status_reason,omitempty" doc:"Why it is in its status, in every answer that is one platform experiment (a read of it and each write that answers it, never a list). Any that is not closed reads repository_creating while the platform creates its repository, before any machine of it starts. A draft's reason is then planning, revising, accepting (proposed, and accepted as proposed at accepts_at unless a person acts first), proposed (it waits for a person to accept) or declined (its planner proposed nothing, for declined_reason, and it waits for a person to revise or close it). An open one's is why it is not running yet: without a baseline baseline_measuring, baseline_required, baseline_undeclared, baseline_declaring or the reason the platform's declaration of the measured job is refused for (budget_below_baseline, baseline_breaks_constraint, baseline_missing_metric), and with one starts_at_ahead, no_researchers, budget_spent, or starting once nothing is missing.A running one's is agents_starting while no researcher's machine is running yet, when the platform closes it at agents_due_at, and running from then. Any that is not closed reads launcher_unreachable instead while a machine it wants, of its planner, coordinator, baseline agents or researchers, is on a launcher that has not asked for its machines for a minute: message names the launcher and how long ago it last asked. A closed one's is its close_reason, or closed on one closed before reasons were kept. message says what the state waits for or how it came about and who acts, the command included, and to an agent that reads an open one it begins with the agent's own next step."`
	// Stages is the elimination ladder, fixed at creation.
	Stages []Stage `json:"stages"`
	// CurrentStage is the 1-based index into Stages of the stage currently running.
	CurrentStage int `json:"current_stage"`
	// Summary is the operator's narrative verdict on the finished run, followed once it closed by
	// the platform's record of the close, written once every job of it ended and was billed. Prose
	// only: the standings are derived from the metrics store on read, never copied here.
	Summary        string   `json:"summary,omitempty"`
	SignedUpAgents []string `json:"signed_up_agents,omitempty"`
	SignupCount    int      `json:"signup_count"`
	// PriorRuns are its prior runs (Tenancy.PriorRuns) the reader reaches, latest close first.
	// Carried by a read of one platform experiment and left out of a list.
	PriorRuns []PriorRun `json:"prior_runs,omitempty" doc:"Every other closed platform experiment of the same project that works on the same repository and ran (its researchers started or it declared a baseline), latest close first. Its agents and its coordinator read each as they read their own, passing its id as platform_experiment_id or --platform-experiment: its summary, standings, jobs and their logs, its hypotheses and findings, and the data its jobs wrote by the url of each object GET /experiments/{id}/data lists. They write nothing to it. Absent when there is none, and on a list."`
	// DefaultQuotaTier is the tier of a signup that does not choose its own.
	DefaultQuotaTier QuotaTier `json:"default_quota_tier,omitempty"`
	// MaxHosted is how many machines the platform may run for this platform experiment, its
	// coordinator included. Fixed at creation.
	MaxHosted int `json:"max_hosted"`
	// Shared is the platform's decision that every account may read this platform experiment and
	// everything in it. Staff set it; nobody outside its org writes to it.
	Shared bool `json:"shared"`

	// CoordinatorHosted means the platform runs this platform experiment's coordinator. Fixed at
	// creation. Coordinator says on which launcher and model and what became of its machine; it
	// is carried by a read of one platform experiment and left out of a list.
	CoordinatorHosted bool               `json:"coordinator_hosted"`
	Coordinator       *HostedCoordinator `json:"coordinator,omitempty"`

	// Repository is the code the platform experiment works on, as the deployment's git host
	// names it, and RepositoryURL the address it is cloned from. AgentImage is the image its hosted agents and
	// coordinator run, empty for the platform's own. All fixed at creation.
	Repository    string `json:"repository,omitempty"`
	RepositoryURL string `json:"repository_url,omitempty" doc:"The HTTPS address of the repository on the git host. A job's code_ref names it."`
	// GitURL is where what the platform launches reaches the repository: the control plane's git
	// proxy, with its own platform token. Set on a read of one platform experiment.
	GitURL string `json:"git_url,omitempty" doc:"Where the agents, the coordinator and the jobs of the platform experiment clone, fetch and push repository_url: the platform's git proxy, which takes their own platform token as the password and refuses a push to any ref but the pusher's own branches. git config url.<git_url>.insteadOf <repository_url> sends git there."`
	// RepositoryAccount is the account the platform creates the repository on, for one created
	// without naming its repository: the repository pass creates it after the create answered
	// (AwaitsRepository). Empty for one that named its repository.
	RepositoryAccount string `json:"-"`
	AgentImage        string `json:"agent_image,omitempty"`

	// BaselineJobID is the completed job that measured the baseline, as the platform checked it
	// when it was declared, and JobEstimatedDurationHours the estimate of a job that states
	// none: the hours that job was observed running, with headroom. A platform experiment
	// starts only with a baseline.
	BaselineJobID             string  `json:"baseline_job_id,omitempty" doc:"The completed job that measured the baseline. Absent until one is declared with PATCH /platform-experiments/{id}. Raised while the run goes on to a stronger job that measured the control (baseline_raises)."`
	JobEstimatedDurationHours float64 `json:"job_estimated_duration_hours,omitzero" doc:"The estimated_duration_hours of a job that states none: one and a half times the hours the baseline job was observed running. Absent without a baseline, and for one too short to be observed alive."`
	// BaselineRaises is every time the baseline was raised while the run went on, oldest first.
	// Carried by a read of one platform experiment and left out of a list.
	BaselineRaises []BaselineRaise `json:"baseline_raises,omitempty" doc:"Each raise of the baseline while the run went on, oldest first: the job declared before and the one after, who raised it and why."`

	// Goal is the sentence a draft was created from, fixed from then on. Empty for a platform
	// experiment that was created whole.
	Goal string `json:"goal,omitempty"`
	// Continues is the closed run it carries on, fixed at creation, empty for one that starts
	// fresh. Its baseline starts from that run's; like every prior run, it is read as its own.
	Continues string `json:"continues,omitempty" doc:"The closed platform experiment of the same project and repository this one carries on, fixed at creation. Absent on one that starts fresh. Its baseline starts from the commit of that run's baseline job (that run's baseline_job_id), so the two are measured by the same harness. It is one of prior_runs, read as every prior run is."`
	// Actor is whose credential created it, unverified with no id on one created before this
	// was kept. Carried by a read of one platform experiment and left out of a list.
	Actor *Actor `json:"actor,omitempty"`
	// A draft's planner answers its goal in one of two ways, and the draft carries the one that
	// stands. Proposal is what it proposed, as the check on writing it took it, and ProposedAt
	// when. DeclinedReason is why it proposed nothing, the goal being none an experiment can be
	// made of, and DeclinedAt when: a decline clears the proposal made before. Both moments are
	// absent while the draft is being planned or revised, when the last proposal or reason stays
	// to be read until the planner answers again. See Answered.
	// Carried by a read of one platform experiment and left out of a list.
	Proposal       json.RawMessage `json:"proposal,omitempty" doc:"The proposal as PUT /platform-experiments/{id}/proposal took it. Kept as proposed once it is accepted."`
	ProposedAt     *time.Time      `json:"proposed_at,omitempty" doc:"On a draft: when its planner proposed what proposal holds and the draft began to wait for a person. Absent while it is planned or revised, and on one its planner declined."`
	DeclinedAt     *time.Time      `json:"declined_at,omitempty" doc:"On a draft: when its planner declined to propose, for declined_reason, and the draft began to wait for a person. Absent while it is planned or revised, and on one that has a proposal."`
	DeclinedReason string          `json:"declined_reason,omitempty" doc:"Why the draft's planner proposed nothing. It stays while a revision is planned and goes with the next proposal."`
	AcceptsAt      *time.Time      `json:"accepts_at,omitempty" doc:"On a proposed draft: when the platform accepts the proposal as proposed, unless a person accepts it sooner, holds it to edit it (POST /platform-experiments/{id}/hold), revises it or closes it. Its baseline agent's machine starts with the proposal and runs while this is set. Absent on a draft that waits for a person."`
	// AcceptedAt is when its proposal was accepted, AcceptedBy whose credential accepted it, nil
	// when the platform did at AcceptsAt, and AcceptedAnswers the answers given to the
	// proposal's questions. Carried by a read of one platform experiment and left out of a list.
	AcceptedAt      *time.Time `json:"accepted_at,omitempty" doc:"When the draft's proposal was accepted, by a person or by the platform at accepts_at. Absent on a draft, on one created whole and on one accepted before this was kept."`
	AcceptedBy      *Actor     `json:"accepted_by,omitempty" doc:"Whose credential accepted the proposal. Absent beside accepted_at when the platform accepted it by itself."`
	AcceptedAnswers []string   `json:"accepted_answers,omitempty" doc:"The answers given to proposal.questions on accepting it, one for each, in their order, as they were added to the brief."`
	// Revisions are what a person asked the planner to change, oldest first. One made after the
	// planner's last answer, or while there is none, is not answered yet.
	Revisions []PlanningRequest `json:"revisions,omitempty"`
	// ClosedAt is when it closed and CloseReason why; ClosedBy is whose credential closed it,
	// absent when the platform did at ClosesAt. A list carries the first two and no ClosedBy.
	ClosedAt    *time.Time  `json:"closed_at,omitempty"`
	CloseReason CloseReason `json:"close_reason,omitempty" enum:"end_time_reached,closed_by_request,planning_limit_reached,proposal_expired,baseline_limit_reached,agents_not_started,planner_stopped,repository_not_created" doc:"Absent while it is not closed, and on one closed before reasons were kept. status_reason says the same with a sentence. planner_stopped is a draft whose planner's machine ended with every restart spent before it answered: the platform restarts a planner that dies, with its session, as it does any agent, and closes the draft in the write that gives the machine up. repository_not_created is a draft or an experiment whose repository the platform could not create after the create answered, and its status_reason message says what the git host answered."`
	ClosedBy    *Actor      `json:"closed_by,omitempty"`
	// CloseMessage is what whoever closed it said of why, which closed_by alone does not say.
	CloseMessage string `json:"close_message,omitempty" doc:"What whoever closed it gave as the reason with POST /platform-experiments/{id}/close, absent when they gave none. status_reason's message and the platform's record of the close in summary repeat it."`
	// RepositoryError is what the git host answered when it did not create the repository of one
	// closed repository_not_created. status_reason carries it.
	RepositoryError string `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ClosesAt is when the platform closes it if nobody does: PlanningEndsAt, ProposalExpiresAt,
// BaselineDueAt or AgentsDueAt, whichever it has, and EndsAt with none of them. It is the one
// moment stored.
func (p *PlatformExperiment) ClosesAt() time.Time {
	switch {
	case !p.PlanningEndsAt.IsZero():
		return p.PlanningEndsAt
	case !p.ProposalExpiresAt.IsZero():
		return p.ProposalExpiresAt
	case !p.BaselineDueAt.IsZero():
		return p.BaselineDueAt
	case !p.AgentsDueAt.IsZero():
		return p.AgentsDueAt
	}
	return p.EndsAt
}

// AwaitsAgents says the platform experiment runs and no researcher is at work yet.
func (p *PlatformExperiment) AwaitsAgents() bool {
	return p.Status == PlatformExpRunning && p.StartedAt.IsZero()
}

// PriorRun is a closed platform experiment of the same project and repository as the one it is
// listed on that ran, with the leading standing on its primary ranking metric.
type PriorRun struct {
	ID          string                   `json:"id"`
	Name        string                   `json:"name"`
	Status      PlatformExperimentStatus `json:"status"`
	ClosedAt    *time.Time               `json:"closed_at,omitempty"`
	CloseReason CloseReason              `json:"close_reason,omitempty"`
	Metric      string                   `json:"metric,omitempty" doc:"Its primary ranking metric, absent when it declared none."`
	Best        *float64                 `json:"best,omitempty" doc:"The leading researcher's best on metric, from the metrics store as GET /platform-experiments/{id}/results ranks it. Absent when nobody ranked."`
	BestJobID   string                   `json:"best_job_id,omitempty" doc:"The job that set best."`
}

// AwaitsRepository says the platform has yet to create the repository it keeps its code in. No
// machine of it is wanted until then.
func (p *PlatformExperiment) AwaitsRepository() bool {
	return p.Repository == "" && p.RepositoryAccount != ""
}

// Answered says a draft's planner has answered its goal, with a proposal or a decline, and the
// draft waits for a person.
func (p *PlatformExperiment) Answered() bool {
	return p.ProposedAt != nil || p.DeclinedAt != nil
}

// SetAnswered records when the planner's answer was stored, nil for none that stands, under the
// name of what the answer was: a decline leaves no proposal.
func (p *PlatformExperiment) SetAnswered(at *time.Time, proposed bool) {
	p.ProposedAt, p.DeclinedAt = nil, nil
	if proposed {
		p.ProposedAt = at
	} else {
		p.DeclinedAt = at
	}
}

// SetClosesAt records at under the name of what ends then, which Status, StartedAt, CloseReason,
// Goal, StartsAt and Answered decide. One with a researcher at work ends its run then. One that
// runs with none, or closed so, has until then for a researcher's machine to be running, and its
// run ends DurationHours after StartsAt. A draft, or one closed as a draft, which was planned
// from a goal and never got a start, is being planned until its planner answers and waits for a
// person from then. Any other is open, or closed before it ran, and has until then to be running.
func (p *PlatformExperiment) SetClosesAt(at time.Time) {
	p.EndsAt, p.AgentsDueAt, p.BaselineDueAt, p.PlanningEndsAt, p.ProposalExpiresAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
	switch {
	case !p.StartedAt.IsZero():
		p.EndsAt = at
	case p.Status == PlatformExpRunning || p.CloseReason == CloseAgentsNotStarted:
		p.AgentsDueAt = at
	case p.Status != PlatformExpDraft && (p.Goal == "" || !p.StartsAt.IsZero()):
		p.BaselineDueAt = at
	case p.Answered():
		p.ProposalExpiresAt = at
	default:
		p.PlanningEndsAt = at
	}
	if p.AwaitsAgents() {
		p.EndsAt = p.StartsAt.Add(time.Duration(p.DurationHours * float64(time.Hour)))
	}
}

// ClockStart is when the baseline of a platform experiment that opens, or whose starts_at is
// stated, at now begins to be measured: stated, or now when that is later or none is stated.
func ClockStart(stated, now time.Time) time.Time {
	if stated.After(now) {
		return stated
	}
	return now
}

// StatusReason says why a platform experiment is in its status: a stable code, the sentence an
// agent acts on (Message), the one a person reads (Summary), and whose move it is (WaitsFor).
type StatusReason struct {
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Summary        string     `json:"summary" doc:"One short plain sentence for a person: what is wrong or happening, what it affects, and who acts. What a UI shows."`
	OperatorDetail string     `json:"operator_detail,omitempty" doc:"What the platform's operator checks, only in answers to staff and only where an operator acts."`
	Since          *time.Time `json:"since,omitempty" doc:"When the condition began, where it is known: for launcher_unreachable the launcher's last check-in."`
	WaitsFor       Waiter     `json:"waits_for,omitempty" enum:"owner,coordinator,platform" doc:"Who acts to change the state, on every status but closed, which waits for nobody. owner: a person of its organisation, and nothing its agents or the platform do changes it (a proposal to accept, a budget below what the measured baseline needs, no researcher, a spent budget). coordinator: its hosted coordinator, and its owner where a person coordinates it, so a person is never told to wait for an agent that does not exist. platform: nobody, since the platform, an agent already at work or the clock brings the next state."`
}

// CloseReason is why a platform experiment closed. Nothing else closes one: a spent budget
// leaves the experiment running to its end time.
type CloseReason string

const (
	// CloseEndTimeReached is a run at its EndsAt.
	CloseEndTimeReached CloseReason = "end_time_reached"
	CloseByRequest      CloseReason = "closed_by_request"
	// ClosePlanningLimitReached is a draft whose planner answered nothing inside the planning
	// limit, and CloseProposalExpired one whose proposal or decline no person took up in time.
	ClosePlanningLimitReached CloseReason = "planning_limit_reached"
	CloseProposalExpired      CloseReason = "proposal_expired"
	// CloseBaselineLimitReached is an open platform experiment that was not running by its
	// BaselineDueAt: nobody declared a baseline for it, or it could not start with one.
	CloseBaselineLimitReached CloseReason = "baseline_limit_reached"
	// CloseAgentsNotStarted is a running platform experiment none of whose researchers' machines
	// was running by its AgentsDueAt.
	CloseAgentsNotStarted CloseReason = "agents_not_started"
	// ClosePlannerStopped is a draft whose planner's machine was given up on, every restart
	// spent, before the planner answered.
	ClosePlannerStopped CloseReason = "planner_stopped"
	// CloseRepositoryNotCreated is one whose repository the git host did not create, for
	// RepositoryError. None of its machines started.
	CloseRepositoryNotCreated CloseReason = "repository_not_created"
)

// PlanningRequest is one request to a draft's planner to change its proposal.
type PlanningRequest struct {
	Message string    `json:"message"`
	Actor   Actor     `json:"actor"`
	At      time.Time `json:"at"`
}

// BaselineRaise is one re-declaration of a running platform experiment's baseline to a job that
// measured the control in a stronger configuration.
type BaselineRaise struct {
	From   string    `json:"from" doc:"The job that was the baseline before."`
	To     string    `json:"to" doc:"The job that is the baseline from then on."`
	Reason string    `json:"reason" doc:"Why, as whoever raised it said."`
	Detail string    `json:"detail" doc:"What every agent was told (platform_experiment.baseline): the value on the ranking metric before and after."`
	Actor  Actor     `json:"actor"`
	At     time.Time `json:"at"`
}

// AgentMessage is one direct message from the coordinator or the owner to an agent of a platform
// experiment, or from the owner to its hosted coordinator, told to its reader as agent.message.
type AgentMessage struct {
	AgentID string    `json:"agent_id" doc:"The agent it was sent to, coordinator for the hosted coordinator."`
	From    string    `json:"from" enum:"coordinator,owner" doc:"coordinator for the platform experiment's coordinator, owner for a person or an API key of its org."`
	Text    string    `json:"text"`
	Actor   Actor     `json:"actor"`
	At      time.Time `json:"at"`
}

// MessageFrom is who an actor's message is from, as its recipient is told.
func MessageFrom(actor Actor) string {
	if actor.Kind == ActorCoordinator {
		return "coordinator"
	}
	return "owner"
}

const (
	// MaxAgentMessageBytes bounds a message's text, which travels whole in its event.
	MaxAgentMessageBytes = 1500
	// MaxAgentMessagesPerHour is how many messages one recipient is sent in any hour by each
	// sender: the owner's count apart from the coordinator's, so an owner's message never spends
	// what the coordinator has to steer with.
	MaxAgentMessagesPerHour = 6
)

// CoordinatorRecipient is the agent_id a message to the hosted coordinator is kept and told under:
// a coordinator has no agent id, and no agent's id is this.
const CoordinatorRecipient = "coordinator"

// MaxGoalBytes bounds the sentence a draft is created from.
const MaxGoalBytes = 2000

// SignupRole is what an agent was signed up to do in one platform experiment. It lives on the
// signup rather than on the agent — the same agent may compete in one platform experiment and
// hold the baseline in another — and is fixed at signup: changing it mid-run would retroactively
// rewrite who a completed cut applied to.
//
// Only ranking reads it. A non-competitor's jobs are admitted, billed, evicted and settled by
// identical code, and its metrics are recorded and readable in full.
type SignupRole string

const (
	// SignupRoleCompetitor is ranked, cut-eligible, takes a quota share and earns the top-3 bonus.
	SignupRoleCompetitor SignupRole = "competitor"
	// SignupRoleBaseline runs the experiment's declared control. Never ranked, never cut — the
	// point of a baseline is that its numbers are visible and comparable, not that it wins. It
	// holds a quota from the moment it joins and is the one role whose job an open platform
	// experiment takes: the baseline is measured before anyone is ranked against it. It takes
	// no share at the start, and what it spent is taken from the budget before the shares.
	SignupRoleBaseline SignupRole = "baseline"
	// SignupRoleReviewer re-checks other agents' claims and records agreement or dispute. Never
	// ranked, never cut.
	SignupRoleReviewer SignupRole = "reviewer"
	// SignupRolePlanner is the agent the platform signs up to plan a draft. It proposes the
	// experiment, runs no job, holds no quota and takes no part once the draft is accepted. No
	// request can ask for it.
	SignupRolePlanner SignupRole = "planner"
)

// BaselineShare is the quota of an agent of role baseline of a platform experiment whose budget
// is budget: half of it. No researcher can spend before the run starts, so measuring the
// baseline draws on the budget they wait for, retries included, and one baseline agent leaves
// them at least half.
func BaselineShare(budget float64) float64 {
	return RoundHours(budget / 2)
}

// ParseSignupRole resolves a requested role. An empty string is the competitor default, so every
// caller written before roles existed keeps meaning what it meant. Anything else unrecognized is
// an error: defaulting a typo to competitor would silently rank an agent nobody meant to rank.
func ParseSignupRole(s string) (SignupRole, error) {
	switch SignupRole(s) {
	case "":
		return SignupRoleCompetitor, nil
	case SignupRoleCompetitor, SignupRoleBaseline, SignupRoleReviewer:
		return SignupRole(s), nil
	}
	return "", fmt.Errorf("unknown role %q: must be one of competitor, baseline, reviewer", s)
}
