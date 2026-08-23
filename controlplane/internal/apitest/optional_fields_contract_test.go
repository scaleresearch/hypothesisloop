package apitest

import (
	"encoding/json"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/services/quota"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/registry"
	"github.com/scaleresearch/hypothesisloop/controlplane/services/scheduler"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/apidocs"
)

// A field added to an existing request body must not become required, and huma makes it required
// by DEFAULT unless the tag says otherwise. Three fields shipped that way and each broke every
// caller written before it existed: `role` on signup rejected every signup, and `author` on the
// hypothesis endpoints rejected every agent-submitted hypothesis and comment. All three were
// meant to be optional -- the design says "competitor is the default, so every existing signup
// still means what it meant" -- and all three passed their unit tests, because a Go test calls
// the service function directly and never crosses the schema that rejects the request.
//
// agent_id and author are a further case: exactly one of them must be set, which is a rule about
// the PAIR. No per-field "required" can express it, and marking either one required rejects the
// other's entirely valid half.
func TestFieldsAddedToExistingRequestBodiesStayOptional(t *testing.T) {
	spec := openAPISpec(t)

	for _, tc := range []struct {
		path, field, why string
	}{
		{"/platform-experiments/{id}/signup", "role", "every signup written before roles existed omits it, and competitor is the documented default"},
		{"/hypotheses", "author", "an agent-submitted hypothesis sets agent_id and no author"},
		{"/hypotheses", "agent_id", "a human-submitted hypothesis sets author and no agent_id"},
		{"/hypotheses/{id}/comments", "author", "an agent-submitted comment sets agent_id and no author"},
		{"/hypotheses/{id}/comments", "agent_id", "a human comment sets author and no agent_id"},
	} {
		required := requestBodyRequired(t, spec, tc.path)
		for _, name := range required {
			if name == tc.field {
				t.Errorf("POST %s requires %q — %s", tc.path, tc.field, tc.why)
			}
		}
	}
}

// openAPISpec serves the document from the same router construction control-service uses, so the
// contract asserted here is the one callers actually meet.
func openAPISpec(t *testing.T) map[string]any {
	t.Helper()
	r := chi.NewRouter()
	doc := apidocs.New(r, "hypothesisloop API", "1.0.0", apidocs.PlatformRules)
	quota.RegisterHuma(doc, quota.NewHandler(nil, nil), quota.NewPlatformExperimentsHandler(nil, nil))
	scheduler.RegisterHuma(doc, scheduler.NewHandler(
		scheduler.NewService(nil, nil, nil, nil, nil, contractSettler{}, "http://metrics.invalid", zap.NewNop()).WithLoop(noopLoop{})))
	registry.RegisterHuma(doc, registry.NewHandler(nil, nil))
	doc.MountExplore(r)

	code, body := get(t, r, "/openapi.json")
	if code != 200 {
		t.Fatalf("GET /openapi.json = %d", code)
	}
	var spec map[string]any
	if err := json.Unmarshal([]byte(body), &spec); err != nil {
		t.Fatalf("decode openapi.json: %v", err)
	}
	return spec
}

// requestBodyRequired returns the required property names of a POST body schema, resolving the
// one $ref level huma emits. An unresolvable path is a failure rather than an empty list: an
// empty list would make this whole test pass vacuously the moment a route is renamed.
func requestBodyRequired(t *testing.T, spec map[string]any, path string) []string {
	t.Helper()
	paths, _ := spec["paths"].(map[string]any)
	op, ok := paths[path].(map[string]any)
	if !ok {
		t.Fatalf("openapi.json has no path %q", path)
	}
	post, ok := op["post"].(map[string]any)
	if !ok {
		t.Fatalf("path %q has no POST operation", path)
	}
	schema := digSchema(t, post)
	if ref, isRef := schema["$ref"].(string); isRef {
		name := ref[len("#/components/schemas/"):]
		components, _ := spec["components"].(map[string]any)
		schemas, _ := components["schemas"].(map[string]any)
		schema, ok = schemas[name].(map[string]any)
		if !ok {
			t.Fatalf("path %q body references unknown schema %q", path, name)
		}
	}
	raw, _ := schema["required"].([]any)
	names := make([]string, 0, len(raw))
	for _, n := range raw {
		if s, isString := n.(string); isString {
			names = append(names, s)
		}
	}
	return names
}

func digSchema(t *testing.T, post map[string]any) map[string]any {
	t.Helper()
	body, ok := post["requestBody"].(map[string]any)
	if !ok {
		t.Fatal("POST operation has no requestBody")
	}
	content, _ := body["content"].(map[string]any)
	media, ok := content["application/json"].(map[string]any)
	if !ok {
		t.Fatal("requestBody has no application/json content")
	}
	schema, ok := media["schema"].(map[string]any)
	if !ok {
		t.Fatal("application/json content has no schema")
	}
	return schema
}

// The same trap one level down, in the job spec. A grouped job REJECTS the top-level per-node
// resource fields -- each group carries its own -- so it cannot send accelerator_count, and huma
// requiring it refused every grouped submission with a validation error naming a field the
// submitter was right to omit.
//
// "Required unless groups is set" is a rule about the whole spec, which ValidateGroups and
// ValidateExperiment own. No per-field "required" can express it, and asserting it here is what
// stops the next field added to JobSpec from quietly re-imposing the ungrouped shape on everyone.
func TestGroupedJobSpecFieldsAreNotSchemaRequired(t *testing.T) {
	spec := openAPISpec(t)

	components, _ := spec["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	jobSpec, ok := schemas["JobSpec"].(map[string]any)
	if !ok {
		t.Fatal("openapi.json has no JobSpec schema")
	}
	raw, _ := jobSpec["required"].([]any)

	// max_retries stays required: it is required of EVERY job, grouped or not, and the design
	// says so deliberately -- an unset retry budget is not a default anyone should inherit.
	forbidden := map[string]string{
		"accelerator_count": "a grouped job rejects the top-level per-node fields, so it omits this",
		"cpu":               "same -- each group carries its own",
		"memory":            "same -- each group carries its own",
		"storage":           "same -- each group carries its own",
		"num_nodes":         "a grouped job rejects num_nodes outright",
		"groups":            "an ungrouped job -- every existing submission -- omits it",
	}
	for _, entry := range raw {
		name, _ := entry.(string)
		if why, bad := forbidden[name]; bad {
			t.Errorf("JobSpec requires %q — %s", name, why)
		}
	}
}
