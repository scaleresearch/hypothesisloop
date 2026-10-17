// Package experimentspec is the one definition of a platform experiment that the CLI, the wizard
// and the API share: the file a repository keeps at File, shaped like the body of
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

// File is where a repository keeps its experiment spec.
const File = ".hypothesisloop/experiment.yaml"

// MaxProseBytes caps the description and the summary: every read of the platform experiment
// answers both whole, and every agent reads it often.
const MaxProseBytes = 32 << 10

// MaxHyperparametersBytes caps the hyperparameters a researcher is started with, as JSON.
const MaxHyperparametersBytes = 4 << 10

// Spec is what a platform experiment is. Its keys are those of the create body.
type Spec struct {
	Name                      string                `json:"name"`
	Description               string                `json:"description"`
	Metrics                   []MetricDefinition    `json:"metrics"`
	BudgetAcceleratorHours    float64               `json:"budget_accelerator_hours"`
	MaxAgents                 int                   `json:"max_agents"`
	DurationHours             float64               `json:"duration_hours"`
	Researchers               []ResearcherPoolEntry `json:"researchers,omitempty"`
	CoordinatorHosted         bool                  `json:"coordinator_hosted,omitempty"`
	CoordinatorModel          string                `json:"coordinator_model,omitempty"`
	ReportIntervalSeconds     int                   `json:"report_interval_seconds,omitempty"`
	Stages                    []Stage               `json:"stages,omitempty"`
	DefaultQuotaTier          string                `json:"default_quota_tier,omitempty"`
	MaxConcurrentAccelerators *int                  `json:"max_concurrent_accelerators,omitempty"`
	AgentImage                string                `json:"agent_image,omitempty"`
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
		return Spec{}, fmt.Errorf("not YAML: %w", err)
	}
	if fields, ok := doc.(map[string]any); ok {
		for _, key := range slices.Sorted(maps.Keys(placement)) {
			if _, ok := fields[key]; ok {
				return Spec{}, fmt.Errorf("%s is set by the request (%s) and has no place in the experiment spec", key, placement[key])
			}
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

// Load reads the spec of the repository at dir.
func Load(dir string) (Spec, error) {
	path := filepath.Join(dir, filepath.FromSlash(File))
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

// Validate refuses the spec by every rule that needs neither the platform's settings nor its
// catalog of agent types, and names every problem it finds.
func (s Spec) Validate() error {
	var problems []error
	refuse := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}
	lines := map[string]string{"name": s.Name, "default_quota_tier": s.DefaultQuotaTier}
	for i, m := range s.Metrics {
		lines[fmt.Sprintf("metrics[%d].key", i)] = m.Key
		lines[fmt.Sprintf("metrics[%d].direction", i)] = m.Direction
		lines[fmt.Sprintf("metrics[%d].role", i)] = string(m.Role)
	}
	for _, field := range slices.Sorted(maps.Keys(lines)) {
		if strings.ContainsAny(strings.TrimSpace(lines[field]), "\r\n") {
			refuse("%s is one line and holds a line break: state it on a single line", field)
		}
	}
	description := strings.TrimSpace(s.Description)
	if strings.TrimSpace(s.Name) == "" || description == "" {
		refuse("name and description are required: the description is the brief every agent works from")
	}
	if len(description) > MaxProseBytes {
		refuse("description is %d bytes and at most %d are kept", len(description), MaxProseBytes)
	}
	if err := ValidateMetricDefinitions(s.Metrics); err != nil {
		refuse("metrics: %w", err)
	} else if err := RequireRankingMetric(s.Metrics); err != nil {
		refuse("metrics: %w", err)
	}
	if s.BudgetAcceleratorHours <= 0 {
		refuse("budget_accelerator_hours is required and must be positive: the cap on everything the run spends")
	}
	if s.DurationHours <= 0 {
		refuse("duration_hours is required and must be positive: how long the run lasts from the moment it starts")
	}
	if s.MaxAgents <= 0 {
		refuse("max_agents is required and must be positive: how many researchers may be at work at once")
	}
	if s.ReportIntervalSeconds < 0 {
		refuse("report_interval_seconds must be positive: how often a job really reports")
	}
	if len(s.Stages) > 0 {
		if err := ValidateStages(s.Stages); err != nil {
			refuse("stages: %w", err)
		} else if len(s.Stages) > 1 && s.MaxAgents > 0 && s.MaxAgents < MinResearchersForCut {
			refuse("stages: %d stages for at most %d researchers, and a run of fewer than %d has one stage since no boundary cuts: state one stage or leave stages out",
				len(s.Stages), s.MaxAgents, MinResearchersForCut)
		}
	}
	if s.DefaultQuotaTier != "" && s.DefaultQuotaTier != "guaranteed" && s.DefaultQuotaTier != "burst_only" {
		refuse("default_quota_tier must be \"guaranteed\" or \"burst_only\"")
	}
	if s.MaxConcurrentAccelerators != nil && *s.MaxConcurrentAccelerators <= 0 {
		refuse("max_concurrent_accelerators must be positive")
	}
	if err := ValidateAgentImage(s.AgentImage); err != nil {
		problems = append(problems, err)
	}
	problems = append(problems, s.validateResearchers()...)
	return errors.Join(problems...)
}

func (s Spec) validateResearchers() []error {
	var problems []error
	seen := map[PoolKey]bool{}
	total := 0
	for i, e := range s.Researchers {
		if e.Count <= 0 {
			problems = append(problems, fmt.Errorf("researchers[%d]: count must be positive, got %d", i, e.Count))
		}
		if encoded, err := json.Marshal(e.Hyperparameters); err != nil || len(encoded) > MaxHyperparametersBytes {
			problems = append(problems, fmt.Errorf("researchers[%d]: hyperparameters must be a JSON object of at most %d bytes", i, MaxHyperparametersBytes))
		}
		if seen[e.Key()] {
			problems = append(problems, fmt.Errorf("researchers[%d]: a second entry of agent_type %q on model %q: state one entry with their count", i, e.AgentType, e.Model))
		}
		seen[e.Key()] = true
		total += e.Count
	}
	if s.MaxAgents > 0 && total > s.MaxAgents {
		problems = append(problems, fmt.Errorf("researchers total %d, more than max_agents %d, the researchers that may be at work at once", total, s.MaxAgents))
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
