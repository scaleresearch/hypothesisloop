package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Repositories reaches GitHub through the installations a project's org connected.
// *accounts.Repositories satisfies it. What either call refuses is returned as the API answers it.
type Repositories interface {
	// Reachable returns the repository as GitHub names it, when an installation the org of the
	// project connected covers it.
	Reachable(ctx context.Context, projectID, repository string) (string, error)
	// ExperimentBase returns the repository's experiment file.
	ExperimentBase(ctx context.Context, projectID, repository string) ([]byte, error)
}

// statedByTheRequest are the fields a create without from_repo must carry, though the schema
// leaves them to the experiment file.
var statedByTheRequest = []string{"name", "description", "metrics", "report_interval_seconds", "starts_at"}

// neverFromTheFile are what one run costs and how long it lasts. Any agent's token can push to
// the branch the file is read from, so these come from the request and from nowhere else.
var neverFromTheFile = []string{"budget_accelerator_hours", "max_agents", "ends_at", "coordinator_hosted", "coordinator_model", "agent_image"}

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
