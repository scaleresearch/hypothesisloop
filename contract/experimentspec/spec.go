// Package experimentspec is the one definition of a platform experiment that the CLI, the wizard
// and the API share: the file a repository keeps at ExperimentFile, shaped like the body of
// POST /platform-experiments without where the experiment is placed.
package experimentspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
)

// MaxProseBytes caps the description and the summary: every read of the platform experiment
// answers both whole, and every agent reads it often.
const MaxProseBytes = 32 << 10

// MaxHyperparametersBytes caps the hyperparameters a researcher is started with, as JSON.
const MaxHyperparametersBytes = 4 << 10

// Spec is what a platform experiment is. Its keys are those of the create body, and its docs
// are that body's.
type Spec struct {
	Name                      string                `json:"name" required:"false"`
	Description               string                `json:"description" required:"false"`
	Metrics                   []MetricDefinition    `json:"metrics" required:"false"`
	BudgetAcceleratorHours    float64               `json:"budget_accelerator_hours"`
	MaxAgents                 int                   `json:"max_agents" doc:"How many researchers may be at work at once: the agents of role competitor, whatever their agent type, that have not finished, were not cut, were not replaced and still have a machine. One that ended leaves its place to another. The coordinator, a draft's planner and agents of role baseline, which measure the baseline before the run and are not ranked, never count, nor do reviewers. At least the total of researchers."`
	DurationHours             float64               `json:"duration_hours" required:"false" doc:"How long the run lasts, in hours from the moment it starts, at most quota.max_platform_experiment_days. Required: 422 invalid_length without it. The time it is open before, while its baseline is measured, is no part of it."`
	Researchers               []ResearcherPoolEntry `json:"researchers,omitempty" doc:"Optional: the researchers it is to have at work, an entry each agent type and model, [{agent_type, model, hyperparameters, count}], at most max_agents in all. A model left out is the platform's default, which the answer shows. 422 invalid_agents for a count below 1, an agent type that is none, a model no agent can be started on, two entries of one agent type and model, or more than max_agents in all. With coordinator_in_agentsandbox the platform creates them with the platform experiment, runs them and keeps their counts at work while the run goes on. The coordinator may add researchers beyond them, up to max_agents."`
	CoordinatorInAgentSandbox bool                  `json:"coordinator_in_agentsandbox,omitempty"`
	CoordinatorModel          string                `json:"coordinator_model,omitempty"`
	ReportIntervalSeconds     int                   `json:"report_interval_seconds,omitempty" doc:"How often a job is expected to report. Left out, it is the platform's scheduler.default_report_interval_seconds."`
	Stages                    []Stage               `json:"stages,omitempty"`
	DefaultQuotaTier          QuotaTier             `json:"default_quota_tier,omitempty"`
	MaxConcurrentAccelerators *int                  `json:"max_concurrent_accelerators,omitempty"`
	AgentImage                string                `json:"agent_image,omitempty" doc:"The image sandboxed agents and the coordinator run, built FROM the platform's agent image so the harness is in it. Empty is the platform's own."`
}

// Problem is one rule a spec breaks, under the key of the spec that breaks it.
type Problem struct {
	Key     string
	Message string
}

func (p Problem) Error() string { return p.Message }

// Problems is every rule a spec breaks.
type Problems []Problem

func (p Problems) Error() string {
	messages := make([]string, len(p))
	for i, problem := range p {
		messages[i] = problem.Message
	}
	return strings.Join(messages, "\n")
}

// placement is what the request that creates an experiment states and its spec never does, with
// the flag of hl that sets each.
var placement = map[string]string{
	"project_id":         "--project",
	"repository":         "--repository",
	"repository_account": "--repository-account",
	"continues":          "--continues",
	"starts_at":          "--starts-at",
}

// Parse reads a spec from YAML. A key the spec does not have is refused by name.
func Parse(data []byte) (Spec, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Spec{}, fmt.Errorf("not YAML: %s", yamlProblem(data, err))
	}
	fields, ok := doc.(map[string]any)
	if !ok || len(fields) == 0 {
		return Spec{}, errors.New("an experiment spec is a YAML mapping of its keys")
	}
	for _, key := range slices.Sorted(maps.Keys(placement)) {
		if _, ok := fields[key]; ok {
			return Spec{}, fmt.Errorf("%s is set by the request (%s) and has no place in the experiment spec", key, placement[key])
		}
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return Spec{}, fmt.Errorf("not an experiment spec: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		return Spec{}, fmt.Errorf("not an experiment spec: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return spec, nil
}

// yamlProblem is why data is not YAML, with where the parser stopped when it says: the line, the
// column and what that line reads.
func yamlProblem(data []byte, err error) string {
	var problem *yaml.LoadError
	if !errors.As(err, &problem) || problem.Mark.Line == 0 {
		return err.Error()
	}
	said := problem.Mark.String() + ": " + problem.Message
	lines := strings.Split(string(data), "\n")
	if problem.Mark.Line > len(lines) || strings.TrimSpace(lines[problem.Mark.Line-1]) == "" {
		return said
	}
	return said + ", where the line reads: " + strings.TrimSpace(lines[problem.Mark.Line-1])
}

// Load reads the spec of the repository at dir.
func Load(dir string) (Spec, error) {
	path := filepath.Join(dir, filepath.FromSlash(ExperimentFile))
	data, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	spec, err := Parse(data)
	if err != nil {
		return Spec{}, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

// Trimmed is the spec with every text of it without the whitespace around it, which a value
// written as a YAML block ends in. Spec's methods take a pointer: an API answer embeds Spec, and
// huma's wrapper of an answer cannot embed a type with methods on its value.
func (spec *Spec) Trimmed() Spec {
	s := *spec
	s.Name, s.Description, s.DefaultQuotaTier = strings.TrimSpace(s.Name), strings.TrimSpace(s.Description), QuotaTier(strings.TrimSpace(string(s.DefaultQuotaTier)))
	s.Metrics = slices.Clone(s.Metrics)
	for i := range s.Metrics {
		m := &s.Metrics[i]
		m.Key, m.Direction, m.Role = strings.TrimSpace(m.Key), strings.TrimSpace(m.Direction), MetricRole(strings.TrimSpace(string(m.Role)))
	}
	return s
}

// Validate refuses the spec, Trimmed, by every rule that needs neither the platform's settings
// nor its catalog of agent types. Its error is Problems, every one it finds. Only the description
// is more than one line.
func (spec *Spec) Validate() error {
	s := spec.Trimmed()
	var problems Problems
	refuse := func(key, format string, args ...any) {
		problems = append(problems, Problem{Key: key, Message: fmt.Sprintf(format, args...)})
	}
	lines := map[string]string{"name": s.Name, "default_quota_tier": string(s.DefaultQuotaTier)}
	for i, m := range s.Metrics {
		lines[fmt.Sprintf("metrics[%d].key", i)] = m.Key
		lines[fmt.Sprintf("metrics[%d].direction", i)] = m.Direction
		lines[fmt.Sprintf("metrics[%d].role", i)] = string(m.Role)
	}
	for _, field := range slices.Sorted(maps.Keys(lines)) {
		if strings.ContainsAny(lines[field], "\r\n") {
			key, _, _ := strings.Cut(field, "[")
			refuse(key, "%s is one line and holds a line break: state it on a single line", field)
		}
	}
	if s.Name == "" || s.Description == "" {
		refuse("name", "name and description are required: the description is the brief every agent works from")
	}
	if len(s.Description) > MaxProseBytes {
		refuse("description", "description is %d bytes and at most %d are kept, since every read of the platform experiment answers it whole: shorten it, and keep the detail in the repository and name the path", len(s.Description), MaxProseBytes)
	}
	if err := ValidateMetricDefinitions(s.Metrics); err != nil {
		refuse("metrics", "metrics: %s", err)
	} else if err := RequireRankingMetric(s.Metrics); err != nil {
		refuse("metrics", "metrics: %s", err)
	}
	if s.BudgetAcceleratorHours <= 0 {
		refuse("budget_accelerator_hours", "budget_accelerator_hours is required and must be positive: the cap on everything the run spends, in accelerator-hours, and in CPU core-hours for work on cores")
	}
	if s.DurationHours <= 0 {
		refuse("duration_hours", "duration_hours is required and must be positive: how long the run lasts from the moment it starts")
	}
	if s.MaxAgents <= 0 {
		refuse("max_agents", "max_agents is required and must be positive: how many researchers (agents of role competitor) may be at work at once")
	}
	if s.ReportIntervalSeconds < 0 {
		refuse("report_interval_seconds", "report_interval_seconds must be positive: how often a job really reports")
	}
	if len(s.Stages) > 0 {
		if err := ValidateStages(s.Stages); err != nil {
			refuse("stages", "stages: %s", err)
		} else if len(s.Stages) > 1 && s.MaxAgents > 0 && s.MaxAgents < MinResearchersForCut {
			refuse("stages", "stages: %d stages for at most %d researchers, and a run of fewer than %d has one stage since no boundary cuts: state one stage or leave stages out",
				len(s.Stages), s.MaxAgents, MinResearchersForCut)
		}
	}
	if s.DefaultQuotaTier != "" && !ValidQuotaTierOverride(string(s.DefaultQuotaTier)) {
		refuse("default_quota_tier", "default_quota_tier must be \"guaranteed\" or \"burst_only\"")
	}
	if s.MaxConcurrentAccelerators != nil && *s.MaxConcurrentAccelerators <= 0 {
		refuse("max_concurrent_accelerators", "max_concurrent_accelerators must be positive")
	}
	if err := ValidateAgentImage(s.AgentImage); err != nil {
		refuse("agent_image", "%s", err)
	}
	for _, problem := range s.researcherProblems() {
		refuse("researchers", "%s: researchers is the researchers it is to have at work, [{agent_type, model, hyperparameters, count}]", problem)
	}
	if len(problems) == 0 {
		return nil
	}
	return problems
}

func (s *Spec) researcherProblems() []string {
	var problems []string
	seen := map[PoolKey]bool{}
	total := 0
	for i, e := range s.Researchers {
		if e.Count <= 0 {
			problems = append(problems, fmt.Sprintf("researchers[%d]: count must be positive, got %d", i, e.Count))
		}
		if encoded, err := json.Marshal(e.Hyperparameters); err != nil || len(encoded) > MaxHyperparametersBytes {
			problems = append(problems, fmt.Sprintf("researchers[%d]: hyperparameters must be a JSON object of at most %d bytes", i, MaxHyperparametersBytes))
		}
		if seen[e.Key()] {
			problems = append(problems, fmt.Sprintf("researchers[%d]: a second entry of agent_type %q on model %q: state one entry with their count", i, e.AgentType, e.Model))
		}
		seen[e.Key()] = true
		total += e.Count
	}
	if s.MaxAgents > 0 && total > s.MaxAgents {
		problems = append(problems, fmt.Sprintf("researchers total %d, more than max_agents %d, the researchers that may be at work at once", total, s.MaxAgents))
	}
	return problems
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
