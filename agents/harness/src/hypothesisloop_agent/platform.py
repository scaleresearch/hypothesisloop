"""The platform's vocabulary the agent harness reads, rendered from the control plane's own
definitions by TestTheHarnessVocabularyIsUnchanged (controlplane/cmd/control-service, go test -run
TestTheHarnessVocabularyIsUnchanged -update). Change those definitions, never this file."""

# The statuses of a job (platformapi.ExperimentStatus), and those it ends in.
EXPERIMENT_STATUSES = frozenset({"CANCELLED", "COMPLETED", "EVICTED", "FAILED", "QUEUED", "REJECTED", "RUNNING", "SUBMITTED"})
ENDED_STATUSES = frozenset({"CANCELLED", "COMPLETED", "EVICTED", "FAILED", "REJECTED"})
STATUS_CANCELLED = "CANCELLED"
STATUS_COMPLETED = "COMPLETED"
STATUS_EVICTED = "EVICTED"
STATUS_FAILED = "FAILED"
STATUS_QUEUED = "QUEUED"
STATUS_REJECTED = "REJECTED"
STATUS_RUNNING = "RUNNING"
STATUS_SUBMITTED = "SUBMITTED"

# The statuses of a platform experiment (domain.PlatformExperimentStatus).
PLATFORM_EXPERIMENT_STATUSES = frozenset({"closed", "draft", "open", "running"})
PLATFORM_EXPERIMENT_CLOSED = "closed"
PLATFORM_EXPERIMENT_DRAFT = "draft"
PLATFORM_EXPERIMENT_OPEN = "open"
PLATFORM_EXPERIMENT_RUNNING = "running"

# The roles the platform serves instructions for (domain.AgentRole).
AGENT_ROLES = frozenset({"baseline", "coordinator", "planner", "researcher"})
ROLE_BASELINE = "baseline"
ROLE_COORDINATOR = "coordinator"
ROLE_PLANNER = "planner"
ROLE_RESEARCHER = "researcher"

# The kinds of change GET /watch carries (domain.EventKinds), and the line hl watch prints in place of changes no longer held.
EVENT_KINDS = frozenset({"agent.activity", "agent.cut", "agent.finished", "agent.idle", "agent.instructions", "agent.message", "agent.replaced", "capacity.idle", "comment.new", "experiment.blocked", "experiment.status", "finding.new", "hypothesis.link", "hypothesis.new", "hypothesis.status", "job.duplicate", "job.held", "job.underperforming", "platform_experiment.baseline", "platform_experiment.description", "platform_experiment.goal", "platform_experiment.plan", "platform_experiment.proposal", "platform_experiment.removed", "platform_experiment.status", "quota.changed", "stage.boundary"})
EVENT_AGENT_ACTIVITY = "agent.activity"
EVENT_AGENT_CUT = "agent.cut"
EVENT_AGENT_FINISHED = "agent.finished"
EVENT_AGENT_IDLE = "agent.idle"
EVENT_AGENT_INSTRUCTIONS = "agent.instructions"
EVENT_AGENT_MESSAGE = "agent.message"
EVENT_AGENT_REPLACED = "agent.replaced"
EVENT_CAPACITY_IDLE = "capacity.idle"
EVENT_COMMENT_NEW = "comment.new"
EVENT_EXPERIMENT_BLOCKED = "experiment.blocked"
EVENT_EXPERIMENT_STATUS = "experiment.status"
EVENT_FINDING_NEW = "finding.new"
EVENT_HYPOTHESIS_LINK = "hypothesis.link"
EVENT_HYPOTHESIS_NEW = "hypothesis.new"
EVENT_HYPOTHESIS_STATUS = "hypothesis.status"
EVENT_JOB_DUPLICATE = "job.duplicate"
EVENT_JOB_HELD = "job.held"
EVENT_JOB_UNDERPERFORMING = "job.underperforming"
EVENT_PLATFORM_EXPERIMENT_BASELINE = "platform_experiment.baseline"
EVENT_PLATFORM_EXPERIMENT_DESCRIPTION = "platform_experiment.description"
EVENT_PLATFORM_EXPERIMENT_GOAL = "platform_experiment.goal"
EVENT_PLATFORM_EXPERIMENT_PLAN = "platform_experiment.plan"
EVENT_PLATFORM_EXPERIMENT_PROPOSAL = "platform_experiment.proposal"
EVENT_PLATFORM_EXPERIMENT_REMOVED = "platform_experiment.removed"
EVENT_PLATFORM_EXPERIMENT_STATUS = "platform_experiment.status"
EVENT_QUOTA_CHANGED = "quota.changed"
EVENT_STAGE_BOUNDARY = "stage.boundary"
WATCH_RESET = "watch.reset"

# What a proxy answers in the platform's place when it cannot reach it, with no JSON (platformapi.GatewayStatuses).
GATEWAY_STATUSES = frozenset({502, 503, 504})

# A token the platform derives, and the kinds of scope it names that run a harness (scope).
DERIVED_TOKEN_PREFIX = "hls_"
SCOPE_AGENT = "agent"
SCOPE_COORDINATOR = "coordinator"

# The lines the launcher reads from a sandboxed harness's output, and the environment it starts the harness with (agentsandbox).
PROGRESS_PREFIX = "hl-progress"
ACTIVITY_PREFIX = "hl-activity"
STATE_PREFIX = "hl-state"
MAX_ACTIVITY_LENGTH = 120
MAX_STATE_LENGTH = 200
PROGRESS_INTERVAL_S = 15
ENV_API_URL = "API_URL"
ENV_TOKEN = "HL_TOKEN"

# How long a token works and a machine stays after its run ended (domain.AgentWindDown, domain.CoordinatorWindDown).
AGENT_WIND_DOWN_S = 120
COORDINATOR_WIND_DOWN_S = 300

# Why a platform experiment closed (domain.AnsweredCloseReasons), why it waits (domain.StatusReasons), why an agent's run ended (domain.RunEnd) and why it was cut.
CLOSE_REASONS = frozenset({"agents_not_started", "baseline_limit_reached", "closed_by_request", "end_time_reached", "planner_stopped", "planning_limit_reached", "proposal_expired", "repository_not_created"})
CLOSE_AGENTS_NOT_STARTED = "agents_not_started"
CLOSE_BASELINE_LIMIT_REACHED = "baseline_limit_reached"
CLOSE_CLOSED_BY_REQUEST = "closed_by_request"
CLOSE_END_TIME_REACHED = "end_time_reached"
CLOSE_PLANNER_STOPPED = "planner_stopped"
CLOSE_PLANNING_LIMIT_REACHED = "planning_limit_reached"
CLOSE_PROPOSAL_EXPIRED = "proposal_expired"
CLOSE_REPOSITORY_NOT_CREATED = "repository_not_created"
STATUS_REASONS = frozenset({"accepting", "agents_starting", "baseline_breaks_constraint", "baseline_declaring", "baseline_measuring", "baseline_missing_metric", "baseline_not_completed", "baseline_not_on_branch", "baseline_required", "baseline_undeclared", "baseline_weaker_than_own_job", "baseline_without_finding", "budget_below_baseline", "budget_spent", "declined", "instance_starting", "no_baseline", "no_researchers", "planning", "proposed", "repository_creating", "researchers_done", "revising", "running", "starting", "starts_at_ahead"})
STATUS_REASON_ACCEPTING = "accepting"
STATUS_REASON_AGENTS_STARTING = "agents_starting"
STATUS_REASON_BASELINE_BREAKS_CONSTRAINT = "baseline_breaks_constraint"
STATUS_REASON_BASELINE_DECLARING = "baseline_declaring"
STATUS_REASON_BASELINE_MEASURING = "baseline_measuring"
STATUS_REASON_BASELINE_MISSING_METRIC = "baseline_missing_metric"
STATUS_REASON_BASELINE_NOT_COMPLETED = "baseline_not_completed"
STATUS_REASON_BASELINE_NOT_ON_BRANCH = "baseline_not_on_branch"
STATUS_REASON_BASELINE_REQUIRED = "baseline_required"
STATUS_REASON_BASELINE_UNDECLARED = "baseline_undeclared"
STATUS_REASON_BASELINE_WEAKER_THAN_OWN_JOB = "baseline_weaker_than_own_job"
STATUS_REASON_BASELINE_WITHOUT_FINDING = "baseline_without_finding"
STATUS_REASON_BUDGET_BELOW_BASELINE = "budget_below_baseline"
STATUS_REASON_BUDGET_SPENT = "budget_spent"
STATUS_REASON_DECLINED = "declined"
STATUS_REASON_INSTANCE_STARTING = "instance_starting"
STATUS_REASON_NO_BASELINE = "no_baseline"
STATUS_REASON_NO_RESEARCHERS = "no_researchers"
STATUS_REASON_PLANNING = "planning"
STATUS_REASON_PROPOSED = "proposed"
STATUS_REASON_REPOSITORY_CREATING = "repository_creating"
STATUS_REASON_RESEARCHERS_DONE = "researchers_done"
STATUS_REASON_REVISING = "revising"
STATUS_REASON_RUNNING = "running"
STATUS_REASON_STARTING = "starting"
STATUS_REASON_STARTS_AT_AHEAD = "starts_at_ahead"
RUN_END_CLOSED = "closed"
RUN_END_CUT = "cut"
RUN_END_FINISHED = "finished"
RUN_END_PLANNED = "planned"
CUT_MAX_AGENTS_LOWERED = "max_agents_lowered"

# The fields of an agent's instructions (platformexperiments.Instructions) and of their settings (domain.AgentTypeSettings).
INSTRUCTIONS_FIELDS = frozenset({"agent_type", "agent_type_version", "digest", "placeholders", "prompt", "role", "rules_version", "settings"})
AGENT_TYPE_SETTINGS_FIELDS = frozenset({"effort", "hyperparameters", "max_subagents", "model"})

# The keys of the state line and the values of its turn (agentsandbox).
STATE_SUBAGENTS = "subagents"
STATE_TURN = "turn"
STATE_SINCE = "since"
STATE_INSTRUCTIONS_DIGEST = "instructions_digest"
STATE_VERSION = "version"
STATE_MODEL_STATUS = "model_status"
STATE_MODEL_TYPE = "model_type"
STATE_MODEL_UTILIZATION = "model_utilization"
STATE_MODEL_RESETS_AT = "model_resets_at"
TURN_WORKING = "working"
TURN_WAITING = "waiting"

# The placeholders the harness fills, the session it stores and the characters of the instructions digest its state line carries (domain).
PLACEHOLDER_HL_COMMANDS = "hl_commands"
PLACEHOLDER_WORKDIR = "workdir"
MAX_SESSION_BYTES = 33554432
INSTRUCTIONS_DIGEST_LENGTH = 12

# The kinds of actor (domain.ActorKinds), who a message of a person or a key is from (domain.MessageFrom) and the value of the first notice of an idle stretch (domain.IdleNew).
ACTOR_AGENT = "agent"
ACTOR_COORDINATOR = "coordinator"
ACTOR_KEY = "key"
ACTOR_STAFF = "staff"
ACTOR_UNVERIFIED = "unverified"
ACTOR_USER = "user"
MESSAGE_FROM_OWNER = "owner"
IDLE_NEW = "new"
