// Package clitest is what the CLI's tests share. Only _test.go files import it.
package clitest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/cli/internal/apiclient"
	"github.com/scaleresearch/hypothesisloop/cli/internal/command"
)

func WithServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

func SetAPIURL(t *testing.T, url string) {
	t.Helper()
	old := os.Getenv("API_URL")
	os.Setenv("API_URL", url)
	t.Cleanup(func() { os.Setenv("API_URL", old) })
	t.Setenv("HL_TOKEN", "test-token")
}

func Field(key string) func(map[string]any) any {
	return func(body map[string]any) any { return body[key] }
}

func Stdin(t *testing.T, content string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	_, _ = w.WriteString(content)
	w.Close()
}

func DerivedToken(scope string) string {
	return "hls_" + base64.RawURLEncoding.EncodeToString([]byte(scope)) + "." + base64.RawURLEncoding.EncodeToString([]byte("mac"))
}

type Recorded struct {
	Method, Path, Query string
	Body                map[string]any
}

// RecordingServer answers every request with reply(n) for the n-th request and records it.
func RecordingServer(t *testing.T, reply func(n int, r Recorded) any) *[]Recorded {
	t.Helper()
	var got []Recorded
	url := WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		rec := Recorded{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.Body); err != nil {
				t.Errorf("request body is not a JSON object: %s", raw)
			}
		}
		got = append(got, rec)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply(len(got)-1, rec))
	})
	SetAPIURL(t, url)
	return &got
}

func WriteFile(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/hypotheses.yaml"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func Lines(s string) []string {
	return strings.Split(strings.TrimSpace(s), "\n")
}

func MustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestBranch is the branch PushedClone leaves HEAD on: jane's implementation of h1 in pe-1.
const TestBranch = "hl/pe-1/jane/idea-abc123"

// PushedClone returns a clone of a local bare origin whose default branch is pushed, with HEAD
// on TestBranch, which origin does not have yet.
func PushedClone(t *testing.T) (clone, origin string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	clone = filepath.Join(root, "clone")
	MustGit(t, root, "init", "-q", "--bare", origin)
	MustGit(t, root, "clone", "-q", origin, clone)
	WriteJob(t, filepath.Join(clone, "job.yaml"), "metadata:\n  hypothesis_id: h1\njob:\n  image: foo\n")
	MustGit(t, clone, "add", ".")
	MustGit(t, clone, "commit", "-q", "-m", "init")
	MustGit(t, clone, "push", "-q", "origin", "HEAD")
	MustGit(t, clone, "checkout", "-q", "-b", TestBranch)
	t.Chdir(clone)
	return clone, origin
}

func WriteJob(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

type RecordedCall struct {
	Method, Path string
	Body         map[string]any
}

// AccountsServer answers every account path with reply and records what it was asked. GET /me
// answers with the given orgs.
func AccountsServer(t *testing.T, orgs []string, reply string) *[]RecordedCall {
	t.Helper()
	calls := &[]RecordedCall{}
	url := WithServer(t, func(w http.ResponseWriter, r *http.Request) {
		call := RecordedCall{Method: r.Method, Path: r.URL.RequestURI()}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &call.Body)
		}
		*calls = append(*calls, call)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/me" {
			me := map[string]any{"kind": "key", "orgs": []map[string]string{}, "projects": []any{}}
			for _, id := range orgs {
				me["orgs"] = append(me["orgs"].([]map[string]string), map[string]string{"id": id, "name": "name of " + id})
			}
			_ = json.NewEncoder(w).Encode(me)
			return
		}
		_, _ = w.Write([]byte(reply))
	})
	SetAPIURL(t, url)
	t.Setenv("HL_ORG", "")
	t.Setenv("HL_PROJECT", "")
	return calls
}

func ReadFile(t *testing.T, clone, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(clone, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Schema is the part of an OpenAPI schema a projection is checked against.
type Schema struct {
	Ref        string             `json:"$ref"`
	Type       any                `json:"type"`
	Properties map[string]*Schema `json:"properties"`
	Items      *Schema            `json:"items"`
	OneOf      []*Schema          `json:"oneOf"`
}

type Document struct {
	Paths map[string]map[string]struct {
		Responses map[string]struct {
			Content map[string]struct {
				Schema *Schema `json:"schema"`
			} `json:"content"`
		} `json:"responses"`
	} `json:"paths"`
	Components struct {
		Schemas map[string]*Schema `json:"schemas"`
	} `json:"components"`
}

func PublishedDocument(t *testing.T) *Document {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(RepoRoot(t), "controlplane", "cmd", "control-service", "testdata", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return &doc
}

func (d *Document) resolve(t *testing.T, s *Schema) *Schema {
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

// Answer is the schema of the success answer of method path.
func (d *Document) Answer(t *testing.T, method, path string) *Schema {
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

func (d *Document) Property(t *testing.T, s *Schema, name string) *Schema {
	t.Helper()
	p := d.resolve(t, s).Properties[name]
	if p == nil {
		t.Fatalf("the schema has no %s", name)
	}
	return d.resolve(t, p)
}

func (d *Document) Items(t *testing.T, s *Schema) *Schema {
	t.Helper()
	s = d.resolve(t, s)
	if s.Items == nil {
		t.Fatalf("the schema is no array")
	}
	return d.resolve(t, s.Items)
}

// types is every JSON type s allows.
func (d *Document) types(t *testing.T, s *Schema) []string {
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

// Project checks that every json field of typ exists in s with a JSON type that decodes into it.
func (d *Document) Project(t *testing.T, at string, typ reflect.Type, s *Schema) {
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
			d.Project(t, at+"[]", typ.Elem(), s.Items)
		}
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if f.Anonymous && name == "" {
				d.Project(t, at, f.Type, s)
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
			d.Project(t, at+"."+name, f.Type, p)
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

// Run runs f as hl runs it, with the client API_URL and HL_TOKEN name.
func Run(f command.Func, args []string, stdout, stderr io.Writer) int {
	return f(apiclient.New(os.Getenv("API_URL"), os.Getenv("HL_TOKEN")), args, stdout, stderr)
}

// Output runs f on args and returns its exit code, stdout and stderr.
func Output(t *testing.T, f command.Func, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(f, args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// RepoRoot is the directory of the module, which holds the CLI and the control plane.
func RepoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func WriteJobFile(t *testing.T) string {
	t.Helper()
	file := t.TempDir() + "/job.yaml"
	if err := os.WriteFile(file, []byte("metadata:\n  code_ref: repo@abc\n  platform_experiment_id: pe-1\n  hypothesis_id: hyp-1\njob:\n  image: foo:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}
