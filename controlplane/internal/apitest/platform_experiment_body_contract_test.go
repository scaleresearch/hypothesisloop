package apitest

import (
	"slices"
	"testing"
)

// The two bodies of a platform experiment require different things, and each set is load-bearing.
//
// An update takes only the fields that change, so it requires none, and what is fixed at
// creation is no field of it.
//
// A create is the experiment spec and its placement. The spec's own validation requires its name,
// description, metrics and length and names every problem of a body in one answer, so the schema
// requires only the budget and max_agents, and from_repo is no field of it.
func TestPlatformExperimentBodiesRequireWhatEachMustCarry(t *testing.T) {
	spec := openAPISpec(t)
	for _, tc := range []struct {
		method, path string
		want         []string
		absent       []string
	}{
		{"post", "/platform-experiments", []string{"budget_accelerator_hours", "max_agents"}, []string{"from_repo"}},
		{"patch", "/platform-experiments/{id}", nil, []string{"from_repo", "project_id", "repository", "branch", "agent_image", "stages", "default_quota_tier"}},
	} {
		schema := bodySchema(t, spec, tc.method, tc.path)
		var required []string
		raw, _ := schema["required"].([]any)
		for _, name := range raw {
			required = append(required, name.(string))
		}
		slices.Sort(required)
		if !slices.Equal(required, tc.want) {
			t.Errorf("%s %s requires %v, want %v", tc.method, tc.path, required, tc.want)
		}
		properties, _ := schema["properties"].(map[string]any)
		for _, name := range tc.absent {
			if _, has := properties[name]; has {
				t.Errorf("%s %s has a field %q", tc.method, tc.path, name)
			}
		}
		if schema["additionalProperties"] != false {
			t.Errorf("%s %s takes fields it does not name, so a field it lacks would pass unread", tc.method, tc.path)
		}
	}
}

func bodySchema(t *testing.T, spec map[string]any, method, path string) map[string]any {
	t.Helper()
	paths, _ := spec["paths"].(map[string]any)
	item, _ := paths[path].(map[string]any)
	op, ok := item[method].(map[string]any)
	if !ok {
		t.Fatalf("openapi.json has no %s %s", method, path)
	}
	schema := digSchema(t, op)
	if ref, isRef := schema["$ref"].(string); isRef {
		components, _ := spec["components"].(map[string]any)
		schemas, _ := components["schemas"].(map[string]any)
		schema, ok = schemas[ref[len("#/components/schemas/"):]].(map[string]any)
		if !ok {
			t.Fatalf("%s %s body references unknown schema %q", method, path, ref)
		}
	}
	return schema
}
