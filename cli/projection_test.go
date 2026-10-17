package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// schema is the part of an OpenAPI schema a projection is checked against.
type schema struct {
	Ref        string             `json:"$ref"`
	Type       any                `json:"type"`
	Properties map[string]*schema `json:"properties"`
	Items      *schema            `json:"items"`
	OneOf      []*schema          `json:"oneOf"`
}

type document struct {
	Paths map[string]map[string]struct {
		Responses map[string]struct {
			Content map[string]struct {
				Schema *schema `json:"schema"`
			} `json:"content"`
		} `json:"responses"`
	} `json:"paths"`
	Components struct {
		Schemas map[string]*schema `json:"schemas"`
	} `json:"components"`
}

func publishedDocument(t *testing.T) *document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "controlplane", "cmd", "control-service", "testdata", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return &doc
}

func (d *document) resolve(t *testing.T, s *schema) *schema {
	t.Helper()
	for s != nil && s.Ref != "" {
		next := d.Components.Schemas[strings.TrimPrefix(s.Ref, "#/components/schemas/")]
		if next == nil {
			t.Fatalf("%s is no schema of the document", s.Ref)
		}
		s = next
	}
	return s
}

// answer is the schema of the success answer of method path.
func (d *document) answer(t *testing.T, method, path string) *schema {
	t.Helper()
	for code, response := range d.Paths[path][strings.ToLower(method)].Responses {
		if strings.HasPrefix(code, "2") {
			if content, ok := response.Content["application/json"]; ok {
				return d.resolve(t, content.Schema)
			}
		}
	}
	t.Fatalf("%s %s answers no JSON", method, path)
	return nil
}

func (d *document) property(t *testing.T, s *schema, name string) *schema {
	t.Helper()
	p := d.resolve(t, s).Properties[name]
	if p == nil {
		t.Fatalf("the schema has no %s", name)
	}
	return d.resolve(t, p)
}

func (d *document) items(t *testing.T, s *schema) *schema {
	t.Helper()
	s = d.resolve(t, s)
	if s.Items == nil {
		t.Fatalf("the schema is no array")
	}
	return d.resolve(t, s.Items)
}

// types is every JSON type s allows.
func (d *document) types(t *testing.T, s *schema) []string {
	t.Helper()
	s = d.resolve(t, s)
	var out []string
	switch v := s.Type.(type) {
	case string:
		out = append(out, v)
	case []any:
		for _, name := range v {
			out = append(out, name.(string))
		}
	}
	for _, branch := range s.OneOf {
		out = append(out, d.types(t, branch)...)
	}
	return out
}

var rawJSON = reflect.TypeFor[json.RawMessage]()

// jsonTypes is the JSON types a value of Go type typ decodes from, nil for any.
func jsonTypes(typ reflect.Type) []string {
	if typ == rawJSON {
		return nil
	}
	switch typ.Kind() {
	case reflect.Pointer:
		return jsonTypes(typ.Elem())
	case reflect.String:
		return []string{"string"}
	case reflect.Bool:
		return []string{"boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return []string{"integer"}
	case reflect.Float32, reflect.Float64:
		return []string{"number", "integer"}
	case reflect.Slice, reflect.Array:
		return []string{"array"}
	case reflect.Map, reflect.Struct:
		return []string{"object"}
	}
	return nil
}

// cliOwn are the fields the CLI prints of its own reckoning, decoded from nothing.
var cliOwn = map[string]bool{"submitted.not_on_best": true}

// project checks that every json field of typ exists in s with a JSON type that decodes into it.
func (d *document) project(t *testing.T, at string, typ reflect.Type, s *schema) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	s = d.resolve(t, s)
	if want := jsonTypes(typ); want != nil {
		got := d.types(t, s)
		if !overlaps(want, got) {
			t.Errorf("%s: the CLI decodes %s from it, and the document says %v", at, typ, got)
			return
		}
	}
	switch typ.Kind() {
	case reflect.Slice, reflect.Array:
		if typ != rawJSON && s.Items != nil {
			d.project(t, at+"[]", typ.Elem(), s.Items)
		}
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if f.Anonymous && name == "" {
				d.project(t, at, f.Type, s)
				continue
			}
			if !f.IsExported() || name == "-" || cliOwn[at+"."+name] {
				continue
			}
			if name == "" {
				name = f.Name
			}
			p := s.Properties[name]
			if p == nil {
				t.Errorf("%s.%s: the CLI reads it, and the answer has no such field", at, name)
				continue
			}
			d.project(t, at+"."+name, f.Type, p)
		}
	}
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// Every field the CLI reads of an answer is one the API documents, of a type it decodes, so a
// field renamed or retyped on the platform fails here and not in an agent's hands.
func TestTheCLIReadsOnlyFieldsTheAPIAnswers(t *testing.T) {
	d := publishedDocument(t)
	job := d.answer(t, "GET", "/experiments/{id}")
	hypothesis := d.answer(t, "GET", "/hypotheses/{id}")
	submit := d.answer(t, "POST", "/experiments")
	for _, c := range []struct {
		name string
		typ  reflect.Type
		in   *schema
	}{
		{"jobBrief", reflect.TypeFor[jobBrief](), job},
		{"briefMetrics", reflect.TypeFor[briefMetrics](), d.property(t, job, "metrics")},
		{"machineTag", reflect.TypeFor[machineTag](), d.property(t, job, "ran_on")},
		{"jobResult", reflect.TypeFor[jobResult](), d.items(t, d.property(t, hypothesis, "jobs"))},
		{"hypothesisBrief", reflect.TypeFor[hypothesisBrief](), hypothesis},
		{"submitted", reflect.TypeFor[submitted](), submit},
		{"runBest", reflect.TypeFor[runBest](), d.property(t, submit, "run_best")},
		{"registeredCluster", reflect.TypeFor[registeredCluster](), d.answer(t, "POST", "/orgs/{id}/clusters")},
		{"declaredExperiment", reflect.TypeFor[declaredExperiment](), d.answer(t, "GET", "/platform-experiments/{id}")},
		{"metricPoint", reflect.TypeFor[metricPoint](), d.items(t, d.answer(t, "GET", "/experiments/{id}/metrics"))},
	} {
		t.Run(c.name, func(t *testing.T) { d.project(t, c.name, c.typ, c.in) })
	}
}
