package domain

import (
	"bytes"
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
)

// AgentRole is what an agent type is for: the part of a run its agents play. A type's role is
// fixed when it is created.
type AgentRole string

const (
	AgentRoleResearcher  AgentRole = "researcher"
	AgentRoleBaseline    AgentRole = "baseline"
	AgentRoleCoordinator AgentRole = "coordinator"
	AgentRolePlanner     AgentRole = "planner"
)

// AgentRoles is every role, in the order they are listed.
var AgentRoles = []AgentRole{AgentRoleResearcher, AgentRoleBaseline, AgentRoleCoordinator, AgentRolePlanner}

// ParseAgentRole refuses a role that is none of AgentRoles.
func ParseAgentRole(s string) (AgentRole, error) {
	if slices.Contains(AgentRoles, AgentRole(s)) {
		return AgentRole(s), nil
	}
	return "", fmt.Errorf("role %q must be one of researcher, baseline, coordinator, planner", s)
}

// RoleOfSignup is the agent role a signup of role r runs as.
func RoleOfSignup(r SignupRole) AgentRole {
	switch r {
	case SignupRoleBaseline:
		return AgentRoleBaseline
	case SignupRolePlanner:
		return AgentRolePlanner
	default:
		return AgentRoleResearcher
	}
}

// DefaultAgentType is the built-in type an agent of role runs as when it names none.
func DefaultAgentType(role AgentRole) string {
	switch role {
	case AgentRoleBaseline:
		return "baseline"
	case AgentRolePlanner:
		return PlannerFlavor
	case AgentRoleCoordinator:
		return CoordinatorFlavor
	default:
		return DefaultAgentFlavor
	}
}

// AgentTypeIDPrefix starts the id of an agent type an org creates. A built-in type's id is its
// name in the repository, which holds no dash.
const AgentTypeIDPrefix = "at"

// AgentType is a named prompt and settings an agent runs with. A built-in one has no org, is
// seeded from the repository at start and is read-only for everyone.
type AgentType struct {
	ID             string     `json:"id"`
	OrgID          string     `json:"org_id,omitempty" doc:"The org that owns it, empty for a built-in type."`
	Name           string     `json:"name"`
	Role           AgentRole  `json:"role" enum:"researcher,baseline,coordinator,planner"`
	Builtin        bool       `json:"builtin" doc:"Seeded by the platform from its repository and read-only for everyone. Duplicate one to change it."`
	CurrentVersion int        `json:"current_version" doc:"The version new agents of the type are pinned to, and live agents are moved to when it is edited."`
	ArchivedAt     *time.Time `json:"archived_at,omitempty" doc:"When it was archived. An archived type takes no new agent; agents pinned to it keep running."`
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
	Digest    string            `json:"digest" doc:"sha256 of prompt and settings. A built-in is seeded again only when its digest matches no version it has."`
	CreatedBy *Actor            `json:"created_by,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// CurrentAgentType is an agent type with its current version.
type CurrentAgentType struct {
	AgentType
	Current AgentTypeVersion `json:"current"`
	// LiveAgents is how many agents and coordinators whose run goes on run the type now.
	LiveAgents int `json:"live_agents" doc:"How many agents and coordinators whose run goes on run the type now: an edit moves each of them to the new version."`
}

// AgentTypeSettings is what an agent type runs with besides its prompt.
type AgentTypeSettings struct {
	Model           string         `json:"model" doc:"The model an agent of the type runs on when the agent names none. Empty: the platform's default."`
	MaxSubagents    int            `json:"max_subagents" doc:"How many subagents run at once, 1 to 10."`
	PacingSubagents int            `json:"pacing_subagents" doc:"How many run at once while the model account is ahead of its pace, 1 to max_subagents."`
	Effort          string         `json:"effort" enum:"low,medium,high,xhigh,max" doc:"How hard the model thinks."`
	Hyperparameters map[string]any `json:"hyperparameters" doc:"What an agent of the type starts from when it is given none, at most 4096 bytes as JSON."`
}

// MaxSubagentsLimit bounds max_subagents.
const MaxSubagentsLimit = 10

var agentEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// Validate refuses settings an agent cannot run with.
func (s AgentTypeSettings) Validate() error {
	if s.Model != "" {
		if err := ValidateModel(s.Model); err != nil {
			return err
		}
	}
	if s.MaxSubagents < 1 || s.MaxSubagents > MaxSubagentsLimit {
		return fmt.Errorf("max_subagents must be 1 to %d, got %d", MaxSubagentsLimit, s.MaxSubagents)
	}
	if s.PacingSubagents < 1 || s.PacingSubagents > s.MaxSubagents {
		return fmt.Errorf("pacing_subagents must be 1 to max_subagents (%d), got %d", s.MaxSubagents, s.PacingSubagents)
	}
	if !slices.Contains(agentEfforts, s.Effort) {
		return fmt.Errorf("effort must be one of %s, got %q", strings.Join(agentEfforts, ", "), s.Effort)
	}
	return validateHyperparameters(s.Hyperparameters)
}

func validateHyperparameters(h map[string]any) error {
	encoded, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("hyperparameters cannot be encoded: %w", err)
	}
	if len(encoded) > MaxHyperparametersBytes {
		return fmt.Errorf("hyperparameters are %d bytes as JSON, the limit is %d", len(encoded), MaxHyperparametersBytes)
	}
	return nil
}

// ValidateModel refuses a model name no agent can be started on.
func ValidateModel(model string) error {
	if len(model) > maxModelLength || !modelPattern.MatchString(model) {
		return fmt.Errorf("model %q must be at most %d characters of letters, digits and . _ : - [ ]", model, maxModelLength)
	}
	return nil
}

// ResolvedModel is the model settings run on: their own, or defaultModel for none.
func (s AgentTypeSettings) ResolvedModel(defaultModel string) string {
	if s.Model != "" {
		return s.Model
	}
	return defaultModel
}

// MaxAgentTypeNameLength and MaxAgentPromptBytes bound what an org saves.
const (
	MaxAgentTypeNameLength = 80
	MaxAgentPromptBytes    = 64 << 10
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
// has no agent id, and only a researcher is given an agent type's name and hyperparameters.
func AgentPlaceholders(role AgentRole) []string {
	all := []string{PlaceholderRules, PlaceholderHLCommands, PlaceholderWorkdir, PlaceholderPlatformExperimentID, PlaceholderRepositoryURL}
	switch role {
	case AgentRoleResearcher:
		return append(all, PlaceholderAgentID, PlaceholderAgentType, PlaceholderHyperparameters)
	case AgentRoleBaseline, AgentRolePlanner:
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
	return CurrentAgentType{}, fmt.Errorf("agent_type %q is not a %s agent type of this org: %s", id, role, strings.Join(names, ", "))
}

// AgentLaunchChoice is what a new agent is started with once ResolveAgentLaunch filled it.
type AgentLaunchChoice struct {
	AgentType       string
	Model           string
	Hyperparameters map[string]any
}

// ResolveAgentLaunch checks what an agent of role asked to be started with and fills what it left
// out, so that a signup always says exactly what its agent runs: the role's built-in type, the
// type's model or else defaultModel, the type's hyperparameters.
func ResolveAgentLaunch(catalog AgentTypeCatalog, role AgentRole, agentType, model, defaultModel string, hyperparameters map[string]any) (AgentLaunchChoice, error) {
	if agentType == "" {
		agentType = DefaultAgentType(role)
	}
	t, err := LookupAgentType(catalog, agentType, role)
	if err != nil {
		return AgentLaunchChoice{}, err
	}
	if model == "" {
		model = t.Current.Settings.ResolvedModel(defaultModel)
	}
	if err := ValidateModel(model); err != nil {
		return AgentLaunchChoice{}, err
	}
	if hyperparameters == nil {
		hyperparameters = t.Current.Settings.Hyperparameters
	}
	if err := validateHyperparameters(hyperparameters); err != nil {
		return AgentLaunchChoice{}, err
	}
	return AgentLaunchChoice{AgentType: agentType, Model: model, Hyperparameters: hyperparameters}, nil
}

// EffectiveAgentSettings is what an agent runs with now: its pinned version's settings with the
// model and hyperparameters its signup holds, which were resolved when it joined and follow the
// type's when the type changes them (PropagatedSettings).
func EffectiveAgentSettings(version AgentTypeSettings, model string, hyperparameters map[string]any) AgentTypeSettings {
	version.Model, version.Hyperparameters = model, nonNilMap(hyperparameters)
	return version
}

// InstructionsDigest is sha256 of what an agent is served: the composed prompt and the settings it
// runs with. The harness compares it with the one it runs to tell whether anything changed.
func InstructionsDigest(prompt string, settings AgentTypeSettings) (string, error) {
	settings.Hyperparameters = nonNilMap(settings.Hyperparameters)
	return AgentTypeDigest(prompt, settings)
}

// ModelDefaults is the platform's model for an agent and for a coordinator that names none.
type ModelDefaults struct {
	Agent       string
	Coordinator string
}

// For is the default of role.
func (d ModelDefaults) For(role AgentRole) string {
	if role == AgentRoleCoordinator {
		return d.Coordinator
	}
	return d.Agent
}

// PinMove is one live agent an edit of its type moved to the new version, or failed to: AgentID is
// CoordinatorRecipient for a coordinator, and empty for a platform experiment's researchers.
type PinMove struct {
	PlatformExperimentID string `json:"platform_experiment_id"`
	AgentID              string `json:"agent_id,omitempty"`
	Error                string `json:"error,omitempty"`
}

// Propagation is what a new version of an agent type did to the agents that run it: each live one
// moved to it, and any that could not be, which stay on the version they had.
type Propagation struct {
	Version int       `json:"version"`
	Moved   []PinMove `json:"moved"`
	Failed  []PinMove `json:"failed"`
}

// Followed is the pool with the entries of agent type typeID that ran on its old model or with its
// old hyperparameters moved to the new ones, as its agents are when the type changes them, and
// entries that come to name one agent type and model merged.
func (p ResearcherPool) Followed(typeID, oldModel, newModel string, oldHyperparameters, newHyperparameters map[string]any) ResearcherPool {
	out := make(ResearcherPool, 0, len(p))
	at := map[PoolKey]int{}
	for _, e := range p {
		if e.AgentType == typeID {
			if e.Model == oldModel {
				e.Model = newModel
			}
			if e.Hyperparameters != nil && sameJSON(e.Hyperparameters, nonNilMap(oldHyperparameters)) {
				e.Hyperparameters = newHyperparameters
			}
		}
		if j, ok := at[e.Key()]; ok {
			out[j].Count += e.Count
			continue
		}
		at[e.Key()] = len(out)
		out = append(out, e)
	}
	return out
}

func sameJSON(a, b any) bool {
	ea, errA := json.Marshal(a)
	eb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ea, eb)
}
