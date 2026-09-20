package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// ProjectRepos reaches the repository attached to a project. *accounts.Repos satisfies it.
type ProjectRepos interface {
	// CloneURL is empty for a project with no repository attached.
	CloneURL(ctx context.Context, projectID string) (string, error)
	// ExperimentBase returns the repository's experiment file. What it refuses is returned as
	// the API answers it.
	ExperimentBase(ctx context.Context, projectID string) ([]byte, error)
}

// WithProjectRepos attaches the repositories platform experiments are created from and cloned.
func (h *PlatformExperimentsHandler) WithProjectRepos(repos ProjectRepos) *PlatformExperimentsHandler {
	h.repos = repos
	return h
}

// fromRepo returns req with every field it leaves out taken from the experiment file of its
// project's repository. What the request sets wins: the file is the base a repository carries,
// and the dates and the budget of one run are the caller's to state.
func (h *PlatformExperimentsHandler) fromRepo(ctx context.Context, req CreatePlatformExperimentRequest) (CreatePlatformExperimentRequest, error) {
	data, err := h.repos.ExperimentBase(ctx, req.ProjectID)
	if err != nil {
		return req, err
	}
	base, err := decodeExperimentBase(data)
	if err != nil {
		return req, invalidPlatformExperiment(err)
	}
	return overlay(base, req), nil
}

// decodeExperimentBase reads the experiment file: YAML shaped like the create body. It cannot
// choose its project, and a key the body does not have is an error, since a mistyped one would
// otherwise be dropped in silence.
func decodeExperimentBase(data []byte) (CreatePlatformExperimentRequest, error) {
	var base CreatePlatformExperimentRequest
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return base, fmt.Errorf("the repository's experiment file is not a YAML mapping: %w", err)
	}
	for _, key := range []string{"project_id", "from_repo"} {
		if _, set := document[key]; set {
			return base, fmt.Errorf("the repository's experiment file must not set %s: the request does", key)
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return base, fmt.Errorf("the repository's experiment file holds a value the create body cannot: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&base); err != nil {
		return base, fmt.Errorf("the repository's experiment file is not shaped like the create body: %w", err)
	}
	return base, nil
}

// overlay returns base with every field request sets laid over it.
func overlay(base, request CreatePlatformExperimentRequest) CreatePlatformExperimentRequest {
	out, over := reflect.ValueOf(&base).Elem(), reflect.ValueOf(request)
	for i := 0; i < over.NumField(); i++ {
		if !over.Field(i).IsZero() {
			out.Field(i).Set(over.Field(i))
		}
	}
	return base
}
