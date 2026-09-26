package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Repositories reaches GitHub through the installations a project's org connected.
// *accounts.Repositories satisfies it. What either call refuses is returned as the API answers it.
type Repositories interface {
	// Reachable returns the repository as GitHub names it, when an installation the org of the
	// project connected covers it.
	Reachable(ctx context.Context, projectID, repository string) (string, error)
	// ExperimentBase returns the repository's experiment file.
	ExperimentBase(ctx context.Context, projectID, repository string) ([]byte, error)
	// Create makes a private repository of that name for a platform experiment that names none,
	// on the GitHub account the org of the project connected, and returns it. It returns nothing
	// on a deployment with no GitHub App, where a platform experiment needs no repository.
	Create(ctx context.Context, projectID, name, description string) (string, error)
}

// RepositoryName is the name of the repository the platform creates for a platform experiment:
// what the experiment is called, cut short, and its id, which keeps two of one name apart.
func RepositoryName(pe *domain.PlatformExperiment) string {
	var slug []byte
	for _, c := range []byte(strings.ToLower(pe.Name)) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			slug = append(slug, c)
		case len(slug) > 0 && slug[len(slug)-1] != '-':
			slug = append(slug, '-')
		}
		if len(slug) == 40 {
			break
		}
	}
	name := strings.Trim(string(slug), "-")
	if name == "" {
		name = "experiment"
	}
	return name + "-" + strings.TrimPrefix(pe.ID, "pe-")
}

// provideRepository gives a platform experiment that names no repository one the platform
// creates for it.
func (h *PlatformExperimentsHandler) provideRepository(ctx context.Context, pe *domain.PlatformExperiment) error {
	if pe.Repository != "" {
		return nil
	}
	created, err := h.repos.Create(ctx, pe.ProjectID, RepositoryName(pe), "Platform experiment "+pe.ID+": "+pe.Name)
	if err != nil {
		return err
	}
	pe.Repository, pe.RepositoryURL = created, domain.RepositoryURL(created)
	return nil
}

// statedByTheRequest are the fields a create without from_repo must carry, though the schema
// leaves them to the experiment file.
var statedByTheRequest = []string{"name", "description", "metrics"}

// neverFromTheFile are what one run costs and how long it lasts. Any agent's token can push to
// the branch the file is read from, so these come from the request and from nowhere else.
var neverFromTheFile = []string{"budget_accelerator_hours", "max_agents", "ends_at", "duration_hours", "coordinator_hosted", "coordinator_model", "agent_image"}

// missing lists the fields of statedByTheRequest the body did not carry.
func (r CreatePlatformExperimentRequest) missing() []string {
	var carried map[string]json.RawMessage
	_ = json.Unmarshal(r.raw, &carried)
	var out []string
	for _, key := range statedByTheRequest {
		if _, ok := carried[key]; !ok {
			out = append(out, key)
		}
	}
	return out
}

// fromRepo returns req laid over the experiment file of the repository it names: every field the
// body carried is the request's, every other one the file's.
func (h *PlatformExperimentsHandler) fromRepo(ctx context.Context, req CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	data, err := h.repos.ExperimentBase(ctx, req.ProjectID, req.Repository)
	if err != nil {
		return req, err
	}
	merged, err := decodeExperimentBase(data)
	if err != nil {
		return req, invalidPlatformExperiment(err)
	}
	if err := json.Unmarshal(req.raw, (*createFields)(&merged)); err != nil {
		return req, fmt.Errorf("lay the request over the experiment file: %w", err)
	}
	return merged, nil
}

// ExperimentFile is what a repository's experiment file states of a platform experiment: the
// fields a create with from_repo would take from it. A form shows them before anything is
// created.
type ExperimentFile struct {
	Repository                string                    `json:"repository"`
	Name                      string                    `json:"name,omitempty"`
	Description               string                    `json:"description,omitempty"`
	Metrics                   []domain.MetricDefinition `json:"metrics,omitempty"`
	ReportIntervalSeconds     int                       `json:"report_interval_seconds,omitempty"`
	Stages                    []domain.Stage            `json:"stages,omitempty"`
	DefaultQuotaTier          string                    `json:"default_quota_tier,omitempty"`
	MaxConcurrentAccelerators *int                      `json:"max_concurrent_accelerators,omitempty"`
}

// experimentFile reads the experiment file of a repository the project's org reaches, by the
// decoding a create with from_repo uses, so that what is shown is what such a create would take.
func (h *PlatformExperimentsHandler) experimentFile(ctx context.Context, projectID, repository string) (*ExperimentFile, error) {
	named, err := h.repos.Reachable(ctx, projectID, repository)
	if err != nil {
		return nil, err
	}
	data, err := h.repos.ExperimentBase(ctx, projectID, named)
	if err != nil {
		return nil, err
	}
	base, err := decodeExperimentBase(data)
	if err != nil {
		return nil, &reasonError{status: http.StatusUnprocessableEntity, Reason: "invalid_experiment_file", Message: domain.ExperimentFile + " of " + named + " cannot be used: " + err.Error()}
	}
	return &ExperimentFile{
		Repository: named, Name: base.Name, Description: base.Description, Metrics: base.Metrics,
		ReportIntervalSeconds: base.ReportIntervalSeconds, Stages: base.Stages,
		DefaultQuotaTier: base.DefaultQuotaTier, MaxConcurrentAccelerators: base.MaxConcurrentAccelerators,
	}, nil
}

// decodeExperimentBase reads the experiment file: YAML shaped like the create body. It cannot
// choose its project, what neverFromTheFile names is left out of it, and a key the body does not
// have is an error, since a mistyped one would otherwise be dropped in silence.
func decodeExperimentBase(data []byte) (CreatePlatformExperimentRequest, error) {
	var base CreatePlatformExperimentRequest
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return base, fmt.Errorf("it is not a YAML mapping: %w", err)
	}
	if len(document) == 0 {
		return base, fmt.Errorf("it is empty")
	}
	for _, key := range []string{"project_id", "from_repo", "repository"} {
		if _, set := document[key]; set {
			return base, fmt.Errorf("it must not set %s: the request does", key)
		}
	}
	for _, key := range neverFromTheFile {
		delete(document, key)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return base, fmt.Errorf("it holds a value the create body cannot, such as a key that is not a string: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode((*createFields)(&base)); err != nil {
		return base, fmt.Errorf("it is not shaped like the create body: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	return base, nil
}
