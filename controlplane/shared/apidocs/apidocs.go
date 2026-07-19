// Package apidocs turns each service's Huma-registered operations into a live,
// self-documenting API surface: an OpenAPI 3.1 document (served by Huma at
// /openapi.json) plus a compact, LLM-friendly Markdown digest served at
// /explore. Agents should fetch /explore at startup instead of reading a
// hand-maintained doc that can drift out of date.
//
// It also pins the platform's error-response shape to the historical
// {"error": "..."} envelope (see init) so existing consumers — the Python
// research agent and the controlplane/ui dashboard — keep working unchanged
// even though Huma defaults to RFC7807 problem+json.
package apidocs

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

// errorModel serializes to the {"error": "..."} shape the Python agent and the
// UI have always parsed, instead of Huma's default RFC7807 problem details.
type errorModel struct {
	status  int
	Message string `json:"error"`
}

func (e *errorModel) Error() string  { return e.Message }
func (e *errorModel) GetStatus() int { return e.status }

func init() {
	// Route every huma.ErrorXXX / huma.NewError through our envelope so the wire
	// shape stays {"error": "..."} for all migrated services.
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		parts := make([]string, 0, len(errs)+1)
		if msg != "" {
			parts = append(parts, msg)
		}
		for _, e := range errs {
			if e != nil {
				parts = append(parts, e.Error())
			}
		}
		return &errorModel{status: status, Message: strings.Join(parts, ": ")}
	}
}

// opMeta is the compact metadata retained per registered operation to render
// the /explore digest.
type opMeta struct {
	Method, Path, Summary, Description string
	Tags                               []string
	Deprecated                         bool
}

// Doc wraps a huma.API and accumulates operation metadata as they are
// registered, so the same registrations drive both /openapi.json and /explore.
type Doc struct {
	API      huma.API
	title    string
	preamble string
	ops      []opMeta
}

// New builds a Huma API over an existing chi router (preserving its middleware,
// /metrics, /healthz and mount paths) and returns a Doc. Huma automatically
// exposes /openapi.json, /openapi.yaml and /docs on the same router.
func New(r chi.Router, title, version, preamble string) *Doc {
	cfg := huma.DefaultConfig(title, version)
	return &Doc{API: humachi.New(r, cfg), title: title, preamble: preamble}
}

// Register registers a Huma operation and records its metadata for the digest.
func Register[I, O any](d *Doc, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	huma.Register(d.API, op, handler)
	d.ops = append(d.ops, opMeta{
		Method:      op.Method,
		Path:        op.Path,
		Summary:     op.Summary,
		Description: op.Description,
		Tags:        op.Tags,
		Deprecated:  op.Deprecated,
	})
}

// MountExplore registers GET /explore on r. It renders one fixed compact
// Markdown digest of every registered operation, grouped by tag.
func (d *Doc) MountExplore(r chi.Router) {
	r.Get("/explore", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(d.render()))
	})
}

func sectionOf(op opMeta) string {
	if len(op.Tags) > 0 {
		return op.Tags[0]
	}
	return "other"
}

func (d *Doc) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — live API reference\n\n", d.title)
	b.WriteString("Generated live from the running service's Huma-registered operations. ")
	b.WriteString("For the full machine-readable schema fetch /openapi.json on this same port.\n")
	b.WriteString("\n")
	if d.preamble != "" {
		b.WriteString(d.preamble)
		b.WriteString("\n")
	}

	bySection := map[string][]opMeta{}
	var sections []string
	for _, op := range d.ops {
		s := sectionOf(op)
		if _, ok := bySection[s]; !ok {
			sections = append(sections, s)
		}
		bySection[s] = append(bySection[s], op)
	}
	sort.Strings(sections)

	for _, s := range sections {
		fmt.Fprintf(&b, "## %s\n\n", s)
		ops := bySection[s]
		sort.SliceStable(ops, func(i, j int) bool {
			if ops[i].Path == ops[j].Path {
				return ops[i].Method < ops[j].Method
			}
			return ops[i].Path < ops[j].Path
		})
		for _, op := range ops {
			dep := ""
			if op.Deprecated {
				dep = " (DEPRECATED)"
			}
			fmt.Fprintf(&b, "- `%s %s`%s — %s\n", op.Method, op.Path, dep, op.Summary)
			if op.Description != "" {
				desc := strings.ReplaceAll(strings.TrimSpace(op.Description), "\n", " ")
				fmt.Fprintf(&b, "  %s\n", desc)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// PlatformRules is the cross-cutting platform-rules preamble (metric-decline
// eviction, silent-eviction, phase-2 holds, no server-side metric validation,
// must-file-summary) that agents need regardless of which endpoint they hit. It
// is embedded in the quota-service /explore (the first service agents talk to).
const PlatformRules = `## platform rules (read first)

- Metric-decline eviction: a job is killed early if no reported metric improves for >=30% of its estimated_duration_hours.
- Silent-eviction: a job is killed if it stops reporting metrics while still RUNNING.
- Phase 2: around 40% of budget spent, agents below the metric cutoff are held (evicted, blocked from resubmitting) for the rest of the run — you cannot know your percentile in advance.
- No server-side validation of reported metric values. Never fabricate or inflate one — it invalidates the experiment for anyone relying on the result.
- File a real summary (POST scheduler /experiments/{id}/summary) after every COMPLETED job, before your next submission.
- A job requesting more of a resource than any node has, or an accelerator_type with no live capacity, QUEUES FOREVER (never errors) — check /resource-catalog/capacity first, and if stuck check GET experiment's not_admitted_reason.
`
