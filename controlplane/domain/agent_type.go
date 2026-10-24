package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
)

// AgentRole is what an agent type is for: the part of a run its agents play. A type's role is
// fixed when it is created.
type AgentRole string

const (
	AgentRoleExecutor    AgentRole = "executor"
	AgentRoleResearcher  AgentRole = "researcher"
	AgentRoleBaseline    AgentRole = "baseline"
	AgentRoleCoordinator AgentRole = "coordinator"
	AgentRolePlanner     AgentRole = "planner"
)

// AgentRoles is every role, in the order they are listed.
var AgentRoles = []AgentRole{AgentRoleExecutor, AgentRoleResearcher, AgentRoleBaseline, AgentRoleCoordinator, AgentRolePlanner}

// AgentRoleInfo says what agents of a role do, for whoever composes a team.
type AgentRoleInfo struct {
	Role        AgentRole  `json:"role"`
	Name        string     `json:"name" doc:"The role's name as it is shown."`
	Description string     `json:"description" doc:"What agents of the role do, in a sentence or two."`
	AgentType   string     `json:"agent_type" doc:"The built-in agent type an agent of the role runs as when it names none."`
	SignupRole  SignupRole `json:"signup_role,omitempty" doc:"The role an agent of it is signed up with. POST /platform-experiments/{id}/agents adds agents of a role in_team and of baseline. Absent for the coordinator, which is not signed up."`
	InTeam      bool       `json:"in_team" doc:"Whether the agents list of a platform experiment names agents of the role: an entry of agent_type researcher is of researchers, and an entry of any type of role executor is of executors."`
	OrgTypes    bool       `json:"org_types" doc:"Whether an org makes agent types of the role (POST /orgs/{id}/agent-types)."`
}

// AgentRoleInfos describes every role, in the order of AgentRoles.
var AgentRoleInfos = []AgentRoleInfo{
	{AgentRoleExecutor, "Executor", "Implements experiments and runs their jobs, makes sure each is implemented correctly and that what it reports is true. Ranked, cut at stage boundaries, holds a quota and counts toward max_agents. Proposing hypotheses is optional.",
		DefaultAgentType, SignupRoleExecutor, true, true},
	{AgentRoleResearcher, "Researcher", "Keeps the hypothesis graph correct and points the work: reads the literature, proposes ideas, reviews outcomes against their metrics, and merges, retires and relinks hypotheses. Runs no job, pushes nothing, holds no quota, is never ranked or cut and counts toward no max_agents. Runs on its own model, else its type's, else the coordinator's.",
		ResearcherAgentType, SignupRoleResearcher, true, false},
	{AgentRoleBaseline, "Baseline", "Measures the experiment's declared control before the run starts, so every executor is ranked against it. Never ranked or cut.",
		"baseline", SignupRoleBaseline, false, true},
	{AgentRoleCoordinator, "Coordinator", "Assembles and steers the team of a platform experiment: one per experiment, not signed up and not ranked.",
		CoordinatorAgentType, "", false, false},
	{AgentRolePlanner, "Planner", "Plans a draft: proposes the experiment, its budget and its team, then takes no part once the draft is accepted.",
		PlannerAgentType, SignupRolePlanner, false, false},
}

// ParseAgentRole refuses a role that is none of AgentRoles.
func ParseAgentRole(s string) (AgentRole, error) {
	if slices.Contains(AgentRoles, AgentRole(s)) {
		return AgentRole(s), nil
	}
	return "", fmt.Errorf("role %q must be one of executor, researcher, baseline, coordinator, planner", s)
}

// Info is what AgentRoleInfos says of role r, the zero AgentRoleInfo for a string that is no role.
func (r AgentRole) Info() AgentRoleInfo {
	for _, info := range AgentRoleInfos {
		if info.Role == r {
			return info
		}
	}
	return AgentRoleInfo{}
}

// RoleOfSignup is the agent role a signup of role r runs as: the one AgentRoleInfos signs up as r,
// and executor for a reviewer, which runs an executor's agent types.
func RoleOfSignup(r SignupRole) AgentRole {
	if r == SignupRoleReviewer {
		return AgentRoleExecutor
	}
	for _, info := range AgentRoleInfos {
		if info.SignupRole == r {
			return info.Role
		}
	}
	return ""
}

// AgentTypeIDPrefix starts the id of an agent type an org creates, followed by a dash. A built-in
// type's id is its name in the repository, which never starts so (ValidateBuiltinAgentTypeID).
const AgentTypeIDPrefix = "at"

var builtinAgentTypeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// ValidateBuiltinAgentTypeID refuses a built-in id that could be an org's: one in the namespace
// of AgentTypeIDPrefix, or not lower-case letters, digits and dashes.
func ValidateBuiltinAgentTypeID(id string) error {
	if !builtinAgentTypeIDPattern.MatchString(id) || strings.HasPrefix(id, AgentTypeIDPrefix+"-") {
		return fmt.Errorf("built-in agent type id %q must be lower-case letters, digits and dashes, and not start with %q, which org types' ids do", id, AgentTypeIDPrefix+"-")
	}
	return nil
}

// OrgAgentRoles is the roles an org makes agent types of (AgentRoleInfo.OrgTypes): a researcher, a
// coordinator or a planner runs only its built-in type yet.
var OrgAgentRoles = func() []AgentRole {
	var roles []AgentRole
	for _, info := range AgentRoleInfos {
		if info.OrgTypes {
			roles = append(roles, info.Role)
		}
	}
	return roles
}()

// CheckOrgAgentRole refuses a role an org cannot make an agent type of.
func CheckOrgAgentRole(role AgentRole) error {
	if !slices.Contains(OrgAgentRoles, role) {
		names := make([]string, len(OrgAgentRoles))
		for i, r := range OrgAgentRoles {
			names[i] = string(r)
		}
		return fmt.Errorf("an org makes agent types of role %s: a %s runs only its built-in type", strings.Join(names, " or "), role)
	}
	return nil
}

// Limits on what an org writes: the agent types it has that are not archived, and the versions an
// edit or a restore makes of one type in an hour.
const (
	MaxOrgAgentTypes        = 100
	MaxAgentTypeEditsAnHour = 60
)

// AgentType is a named prompt and settings an agent runs with. A built-in one has no org, is
// seeded from the repository at start and is read-only for everyone.
type AgentType struct {
	ID             string     `json:"id"`
	OrgID          string     `json:"org_id,omitempty" doc:"The org that owns it, empty for a built-in type."`
	Name           string     `json:"name"`
	Description    string     `json:"description" doc:"One line on what agents of the type do, at most 200 characters. Empty when none was written."`
	Role           AgentRole  `json:"role"`
	Builtin        bool       `json:"builtin" doc:"Seeded by the platform from its repository and read-only for everyone. Duplicate one to change it."`
	CurrentVersion int        `json:"current_version" doc:"The version every agent of the type runs: an edit or a restore makes the next one current, and each live agent is told with agent.instructions."`
	ArchivedAt     *time.Time `json:"archived_at,omitempty" doc:"When it was archived. An archived type takes no new agent; agents that run it keep running."`
	CreatedBy      *Actor     `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// AgentTypeVersion is one immutable version of an agent type: its prompt and settings as they were
// saved. An edit inserts the next version; none is ever changed or deleted.
type AgentTypeVersion struct {
	TypeID    string            `json:"agent_type"`
	Version   int               `json:"version"`
	Prompt    string            `json:"prompt" doc:"The system prompt, with {{name}} placeholders for the values filled at run time (AgentPlaceholders)."`
	Settings  AgentTypeSettings `json:"settings"`
	Digest    string            `json:"digest" doc:"sha256 of prompt and settings."`
	Release   *int              `json:"release,omitempty" doc:"For a built-in version, the platform release that seeded it."`
	CreatedBy *Actor            `json:"created_by,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// AgentTypeChange is what a change did to an agent type.
type AgentTypeChange string

const (
	AgentTypeCreated   AgentTypeChange = "create"
	AgentTypeEdited    AgentTypeChange = "edit"
	AgentTypeRenamed   AgentTypeChange = "rename"
	AgentTypeDescribed AgentTypeChange = "describe"
	AgentTypeArchived  AgentTypeChange = "archive"
	AgentTypeRestored  AgentTypeChange = "restore"
)

// AgentTypeChanges is everything a change can do to an agent type.
var AgentTypeChanges = []AgentTypeChange{AgentTypeCreated, AgentTypeEdited, AgentTypeRenamed, AgentTypeDescribed, AgentTypeArchived, AgentTypeRestored}

// AgentTypeAction is something the caller may do to an agent type now.
type AgentTypeAction string

const (
	AgentTypeActionEdit      AgentTypeAction = "edit"
	AgentTypeActionArchive   AgentTypeAction = "archive"
	AgentTypeActionDuplicate AgentTypeAction = "duplicate"
	AgentTypeActionRestore   AgentTypeAction = "restore"
)

var AgentTypeActions = []AgentTypeAction{AgentTypeActionEdit, AgentTypeActionArchive, AgentTypeActionDuplicate, AgentTypeActionRestore}

// AgentTypeEvent is one change of an agent type, as it is kept: none is ever changed or deleted.
type AgentTypeEvent struct {
	ID      int64           `json:"id"`
	TypeID  string          `json:"agent_type"`
	Action  AgentTypeChange `json:"action"`
	Version int             `json:"version" doc:"The version the type was at after the change: the one an edit or a restore made."`
	Detail  string          `json:"detail" doc:"The new name of a rename, the new description of a describe, the version a restore copied, the release that seeded a built-in edit; empty otherwise."`
	By      *Actor          `json:"by,omitempty" doc:"Whose credential made the change: staff for the platform's seeding of a built-in type."`
	At      time.Time       `json:"at"`
}

// CurrentAgentType is an agent type with its current version.
type CurrentAgentType struct {
	AgentType
	Current AgentTypeVersion `json:"current"`
}

// AgentTypeSettings is what an agent type runs with besides its prompt.
type AgentTypeSettings struct {
	Model           string         `json:"model" required:"false" doc:"The model an agent of the type runs on when the agent names none, read whenever the agent is served: <provider>/<model id>, one GET /models lists. Empty or left out: the platform's default."`
	MaxSubagents    int            `json:"max_subagents" doc:"How many subagents run at once, 1 to 10."`
	Effort          string         `json:"effort" enum:"low,medium,high,xhigh,max" doc:"How hard the model thinks."`
	Hyperparameters map[string]any `json:"hyperparameters" required:"false" doc:"What an agent of the type starts from when it is given none, read whenever the agent is served, at most 4096 bytes as JSON. Left out: none."`
}

// MaxSubagentsLimit bounds max_subagents.
const MaxSubagentsLimit = 10

var agentEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// Validate refuses settings an agent cannot run with.
func (s AgentTypeSettings) Validate() error {
	if s.Model != "" {
		if _, err := ParseModelRef(s.Model); err != nil {
			return err
		}
	}
	if s.MaxSubagents < 1 || s.MaxSubagents > MaxSubagentsLimit {
		return fmt.Errorf("max_subagents must be 1 to %d, got %d", MaxSubagentsLimit, s.MaxSubagents)
	}
	if !slices.Contains(agentEfforts, s.Effort) {
		return fmt.Errorf("effort must be one of %s, got %q", strings.Join(agentEfforts, ", "), s.Effort)
	}
	return experimentspec.ValidateHyperparameters(s.Hyperparameters)
}

// MaxAgentTypeNameLength, MaxAgentTypeDescriptionLength and MaxAgentPromptBytes bound what an org saves.
const (
	MaxAgentTypeNameLength        = 80
	MaxAgentTypeDescriptionLength = 200
	MaxAgentPromptBytes           = 64 << 10
)

// The placeholders a prompt holds. The platform fills the assignment's when it serves the prompt
// (ComposePrompt); the harness fills HarnessPlaceholders, which only the machine knows.
const (
	PlaceholderRules                = "rules"
	PlaceholderHLCommands           = "hl_commands"
	PlaceholderWorkdir              = "workdir"
	PlaceholderAgentID              = "agent_id"
	PlaceholderPlatformExperimentID = "platform_experiment_id"
	PlaceholderAgentType            = "agent_type"
	PlaceholderHyperparameters      = "hyperparameters"
	PlaceholderRepositoryURL        = "repository_url"
)

// HarnessPlaceholders are left in the prompt the platform serves: hl's listing of its commands,
// taken from the hl on the machine, and the directory the agent works in.
var HarnessPlaceholders = []string{PlaceholderHLCommands, PlaceholderWorkdir}

// RequiredPlaceholders are in every prompt: the platform's rules and hl's command listing are what
// an agent cannot work without.
var RequiredPlaceholders = []string{PlaceholderRules, PlaceholderHLCommands}

// AgentPlaceholders is what a prompt of role may hold. A coordinator is no agent of the run, so it
// has no agent id, and only an executor is given an agent type's name and hyperparameters.
func AgentPlaceholders(role AgentRole) []string {
	all := []string{PlaceholderRules, PlaceholderHLCommands, PlaceholderWorkdir, PlaceholderPlatformExperimentID, PlaceholderRepositoryURL}
	switch role {
	case AgentRoleExecutor:
		return append(all, PlaceholderAgentID, PlaceholderAgentType, PlaceholderHyperparameters)
	case AgentRoleResearcher, AgentRoleBaseline, AgentRolePlanner:
		return append(all, PlaceholderAgentID)
	default:
		return all
	}
}

var (
	placeholderPattern = regexp.MustCompile(`\{\{([a-z_]+)\}\}`)
	placeholderOpen    = regexp.MustCompile(`\{\{|\}\}`)
)

// ValidatePrompt refuses a prompt an agent of role would break on: not UTF-8, empty or too long,
// missing a RequiredPlaceholders, holding a placeholder role does not have, or a {{ or }} that is
// no placeholder. Nothing else in a prompt is ever expanded.
func ValidatePrompt(role AgentRole, prompt string) error {
	if !utf8.ValidString(prompt) {
		return fmt.Errorf("prompt is not UTF-8")
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("prompt is empty")
	}
	if len(prompt) > MaxAgentPromptBytes {
		return fmt.Errorf("prompt is %d bytes, the limit is %d", len(prompt), MaxAgentPromptBytes)
	}
	allowed := AgentPlaceholders(role)
	found := map[string]bool{}
	for _, m := range placeholderPattern.FindAllStringSubmatch(prompt, -1) {
		if !slices.Contains(allowed, m[1]) {
			return fmt.Errorf("prompt holds {{%s}}, which a %s prompt has no value for: it may hold %s", m[1], role, placeholderList(allowed))
		}
		found[m[1]] = true
	}
	if stray := placeholderOpen.FindString(placeholderPattern.ReplaceAllString(prompt, "")); stray != "" {
		return fmt.Errorf("prompt holds %s outside a placeholder: a placeholder is {{name}} with name one of %s", stray, placeholderList(allowed))
	}
	for _, name := range RequiredPlaceholders {
		if !found[name] {
			return fmt.Errorf("prompt must hold {{%s}}", name)
		}
	}
	return nil
}

func placeholderList(names []string) string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = "{{" + name + "}}"
	}
	return strings.Join(out, ", ")
}

// ValidateAgentTypeName refuses an empty or overlong name.
func ValidateAgentTypeName(name string) error {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > MaxAgentTypeNameLength {
		return fmt.Errorf("name must be 1 to %d characters", MaxAgentTypeNameLength)
	}
	return nil
}

// ValidateAgentTypeDescription refuses a description of more than one line or over
// MaxAgentTypeDescriptionLength characters. An empty one is allowed.
func ValidateAgentTypeDescription(description string) error {
	if strings.ContainsAny(description, "\r\n") || utf8.RuneCountInString(description) > MaxAgentTypeDescriptionLength {
		return fmt.Errorf("description must be one line of at most %d characters", MaxAgentTypeDescriptionLength)
	}
	return nil
}

// AgentTypeDigest is sha256 of a version's prompt and settings.
func AgentTypeDigest(prompt string, settings AgentTypeSettings) (string, error) {
	encoded, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("encode settings: %w", err)
	}
	sum := sha256.New()
	sum.Write([]byte(prompt))
	sum.Write([]byte{0})
	sum.Write(encoded)
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// Assignment is what the platform fills a prompt's placeholders with for one agent.
type Assignment struct {
	Rules                string
	AgentID              string
	PlatformExperimentID string
	AgentType            string
	RepositoryURL        string
	Hyperparameters      map[string]any
}

// ComposePrompt fills prompt's placeholders with a's values in one pass, so that no value is
// expanded again, and leaves HarnessPlaceholders for the harness.
func ComposePrompt(prompt string, a Assignment) (string, error) {
	hyperparameters, err := pythonJSON(nonNilMap(a.Hyperparameters))
	if err != nil {
		return "", fmt.Errorf("compose prompt: hyperparameters: %w", err)
	}
	return strings.NewReplacer(
		"{{"+PlaceholderRules+"}}", strings.TrimSpace(a.Rules),
		"{{"+PlaceholderAgentID+"}}", a.AgentID,
		"{{"+PlaceholderPlatformExperimentID+"}}", a.PlatformExperimentID,
		"{{"+PlaceholderAgentType+"}}", a.AgentType,
		"{{"+PlaceholderRepositoryURL+"}}", a.RepositoryURL,
		"{{"+PlaceholderHyperparameters+"}}", hyperparameters,
	).Replace(prompt), nil
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// pythonJSON encodes v as Python's json.dumps does with its defaults, which is how an agent's
// hyperparameters were always shown to it: ", " and ": " between items, non-ASCII escaped, keys
// in the order the harness read them, which for an object from the platform is sorted.
func pythonJSON(v any) (string, error) {
	var b strings.Builder
	if err := writePythonJSON(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writePythonJSON(b *strings.Builder, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			writePythonString(b, k)
			b.WriteString(": ")
			if err := writePythonJSON(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := writePythonJSON(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case string:
		writePythonString(b, t)
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e21 {
			b.WriteString(strconv.FormatFloat(t, 'f', -1, 64))
		} else {
			b.WriteString(pythonFloat(t))
		}
	case nil, bool:
		encoded, _ := json.Marshal(t)
		b.Write(encoded)
	default:
		return fmt.Errorf("%T is no JSON value", v)
	}
	return nil
}

// pythonFloat is repr of a float as Python writes it: the shortest form that reads back, in an
// exponent form written e-05 below 1e-4 and from 1e16.
func pythonFloat(f float64) string {
	mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	n, _ := strconv.Atoi(exponent)
	if n < -4 || n >= 16 {
		sign := "+"
		if n < 0 {
			sign, n = "-", -n
		}
		return fmt.Sprintf("%se%s%02d", mantissa, sign, n)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

func writePythonString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || (r > 0x7e && r <= 0xffff):
			fmt.Fprintf(b, `\u%04x`, r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// AgentTypeCatalog is the agent types an org's agents may run as: the built-in ones and the org's
// own, each with its current version, as the store read them.
type AgentTypeCatalog []CurrentAgentType

// LookupAgentType is the one place an agent type is resolved for a new agent: the type of id in
// catalog, of role, and not archived.
func LookupAgentType(catalog AgentTypeCatalog, id string, role AgentRole) (CurrentAgentType, error) {
	var names []string
	for _, t := range catalog {
		if t.Role != role || t.ArchivedAt != nil {
			continue
		}
		if t.ID == id {
			return t, nil
		}
		names = append(names, t.ID)
	}
	return CurrentAgentType{}, fmt.Errorf("agent_type names no %s agent type of this org: %s", role, strings.Join(names, ", "))
}

// AgentLaunchChoice is what a new agent is started with once ResolveAgentLaunch checked it. An
// empty Model and nil Hyperparameters are the type's, read whenever the agent is served.
type AgentLaunchChoice struct {
	AgentType       string
	Model           string
	Hyperparameters map[string]any
}

// LaunchError is what ResolveAgentLaunch refuses, Field naming agent_type, model or
// hyperparameters.
type LaunchError struct {
	Field string
	Err   error
}

func (e *LaunchError) Error() string { return e.Err.Error() }
func (e *LaunchError) Unwrap() error { return e.Err }

// ResolveAgentLaunch checks what an agent of role asked to be started with: the role's built-in
// type for none, a model reference and hyperparameters of the size allowed.
// What it leaves out stays out, and is the type's.
func ResolveAgentLaunch(catalog AgentTypeCatalog, role AgentRole, agentType, model string, hyperparameters map[string]any) (AgentLaunchChoice, error) {
	if agentType == "" {
		agentType = role.Info().AgentType
	}
	if _, err := LookupAgentType(catalog, agentType, role); err != nil {
		return AgentLaunchChoice{}, &LaunchError{Field: "agent_type", Err: err}
	}
	if model != "" {
		if _, err := ParseModelRef(model); err != nil {
			return AgentLaunchChoice{}, &LaunchError{Field: "model", Err: err}
		}
	}
	if err := experimentspec.ValidateHyperparameters(hyperparameters); err != nil {
		return AgentLaunchChoice{}, &LaunchError{Field: "hyperparameters", Err: err}
	}
	return AgentLaunchChoice{AgentType: agentType, Model: model, Hyperparameters: hyperparameters}, nil
}

// TypeModel is the model the current version of agentType in catalog names, empty for none.
func TypeModel(catalog AgentTypeCatalog, agentType string) string {
	for _, t := range catalog {
		if t.ID == agentType {
			return t.Current.Settings.Model
		}
	}
	return ""
}

// EffectiveAgentSettings is what an agent runs with now: the settings of its type's current
// version, on model, the one its ModelChoice resolves, and with the hyperparameters of its own
// where it names them.
func EffectiveAgentSettings(version AgentTypeSettings, model string, hyperparameters map[string]any) AgentTypeSettings {
	if hyperparameters == nil {
		hyperparameters = version.Hyperparameters
	}
	version.Model, version.Hyperparameters = model, nonNilMap(hyperparameters)
	return version
}

// InstructionsDigest is sha256 of what an agent is served: the composed prompt, the settings it
// runs with and the version of the platform's rules the prompt holds. The harness compares it with
// the one it runs to tell whether anything changed.
func InstructionsDigest(prompt string, settings AgentTypeSettings, rulesVersion int) (string, error) {
	settings.Hyperparameters = nonNilMap(settings.Hyperparameters)
	return AgentTypeDigest(fmt.Sprintf("%s\x00rules %d", prompt, rulesVersion), settings)
}

// ModelDefaults is the platform's model for an agent and for a coordinator that names none. A
// researcher steers the run as the coordinator does, so its default is the coordinator's.
type ModelDefaults struct {
	Agent       string
	Coordinator string
}

// For is the default of role.
func (d ModelDefaults) For(role AgentRole) string {
	if role == AgentRoleCoordinator || role == AgentRoleResearcher {
		return d.Coordinator
	}
	return d.Agent
}

// ModelChoice is what the model of an agent or a coordinator resolves from, each empty for none:
// its own, its type's, and the coordinator_model of its platform experiment.
type ModelChoice struct {
	Role                   AgentRole
	Own, Type, Coordinator string
}

// Model is the model the choice runs on: its own, else its type's, else for a researcher the
// coordinator's, else the platform's default of its role.
func (c ModelChoice) Model(defaults ModelDefaults) string {
	switch {
	case c.Own != "":
		return c.Own
	case c.Type != "":
		return c.Type
	case c.Role == AgentRoleResearcher && c.Coordinator != "":
		return c.Coordinator
	}
	return defaults.For(c.Role)
}

// Propagation is what a new version of an agent type told the agents that run it: every agent
// and coordinator whose run goes on runs it from its next turn boundary.
type Propagation struct {
	Version int `json:"version"`
	Told    int `json:"told" doc:"The agents and coordinators whose run goes on told with agent.instructions."`
}

// BuiltinAgentType is one built-in agent type: its id, which is also the file its prompt is in,
// name, role and settings.
type BuiltinAgentType struct {
	ID          string
	Name        string
	Description string
	Role        AgentRole
	Prompt      string
	Settings    AgentTypeSettings
}
