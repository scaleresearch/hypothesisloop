package command

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v4"
)

// YAMLProblem is why raw is not the YAML a command reads, with where the parser stopped when
// it says: the line, the column and what that line reads.
func YAMLProblem(raw []byte, err error) string {
	var problem *yaml.LoadError
	if !errors.As(err, &problem) || problem.Mark.Line == 0 {
		return err.Error()
	}
	said := problem.Mark.String() + ": " + problem.Message
	lines := strings.Split(string(raw), "\n")
	if problem.Mark.Line > len(lines) || strings.TrimSpace(lines[problem.Mark.Line-1]) == "" {
		return said
	}
	return said + ", where the line reads: " + strings.TrimSpace(lines[problem.Mark.Line-1])
}

func ReadYAMLBody(path string) (map[string]any, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var body map[string]any
	if err := yaml.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML: %s", path, YAMLProblem(raw, err))
	}
	if body == nil {
		return nil, fmt.Errorf("expected a YAML mapping at the top level, got nothing")
	}
	return body, nil
}

// ReadYAMLItems reads the hypotheses of FILE (or stdin for "-"), one map each: a bare mapping is
// one, a bare list of mappings one per element, and a mapping holding a listKey list one per
// element of it, with every other top-level key the default of an item that does not set it.
func ReadYAMLItems(path, listKey string) ([]map[string]any, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s is not valid YAML: %s", path, YAMLProblem(raw, err))
	}
	switch v := doc.(type) {
	case []any:
		return toMapSlice(v)
	case map[string]any:
		if list, ok := v[listKey].([]any); ok {
			items, err := toMapSlice(list)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", listKey, err)
			}
			defaults := make(map[string]any, len(v))
			for k, val := range v {
				if k != listKey {
					defaults[k] = val
				}
			}
			for _, item := range items {
				for k, val := range defaults {
					if _, exists := item[k]; !exists {
						item[k] = val
					}
				}
			}
			return items, nil
		}
		if len(v) == 0 {
			return nil, fmt.Errorf("expected a YAML mapping or list at the top level, got nothing")
		}
		return []map[string]any{v}, nil
	default:
		return nil, fmt.Errorf("expected a YAML mapping or list at the top level")
	}
}

func toMapSlice(list []any) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(list))
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d: expected a YAML mapping, got %T", i, e)
		}
		out = append(out, m)
	}
	return out, nil
}
