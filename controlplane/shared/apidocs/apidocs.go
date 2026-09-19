// Package apidocs turns each service's Huma-registered operations into a live,
// self-documenting API surface: an OpenAPI 3.1 document (served by Huma at
// /openapi.json) plus a compact, LLM-friendly Markdown digest served at
// /explore, so nobody reads a hand-maintained doc that can drift out of date.
//
// Every Register call declares an Audience (agent/coordinator/internal) — required, no
// default — so /explore can be rendered per audience from the same registrations
// (MountExploreAudience) and a research agent's own digest can never contain an operation it
// isn't permitted to call.
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

// Audience classifies who is permitted to call a registered operation, so a per-audience
// /explore digest can never leak an operation that audience must not use. Required on every
// Register call — there is no default, so an operation cannot be added without classifying it.
type Audience string

const (
	// AudienceAgent: research agents and hl, their client — it must contain nothing an agent
	// shouldn't call.
	AudienceAgent Audience = "agent"
	// AudienceCoordinator: the coordinator/operator/dashboard — experiment lifecycle admin
	// (create/start/close/summary), donation administration, and other operator-only actions.
	AudienceCoordinator Audience = "coordinator"
	// AudienceInternal: cluster-agent binaries only (the reconcile/status-push contract) —
	// never fetched by a research agent or the coordinator.
	AudienceInternal Audience = "internal"
)

func validAudience(a Audience) bool {
	switch a {
	case AudienceAgent, AudienceCoordinator, AudienceInternal:
		return true
	default:
		return false
	}
}

// opMeta is the compact metadata retained per registered operation to render
// the /explore digest.
type opMeta struct {
	Method, Path, Summary, Description string
	Tags                               []string
	Deprecated                         bool
	Audience                           Audience
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
	// A filter the server doesn't know is a filter that wasn't applied, and returning unfiltered
	// data for it is indistinguishable from a correct answer — a caller filtering by a mistyped or
	// imagined param silently reads the whole table and believes it. 400 instead, naming the param.
	cfg.RejectUnknownQueryParameters = true
	return &Doc{API: humachi.New(r, cfg), title: title, preamble: preamble}
}

// Register registers a Huma operation and records its metadata for the digest. audience is
// required (see Audience) — Register panics on an invalid one, so a call site cannot add an
// operation without classifying who may call it.
func Register[I, O any](d *Doc, audience Audience, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	if !validAudience(audience) {
		panic(fmt.Sprintf("apidocs.Register %s %s: invalid audience %q", op.Method, op.Path, audience))
	}
	huma.Register(d.API, op, handler)
	d.ops = append(d.ops, opMeta{
		Method:      op.Method,
		Path:        op.Path,
		Summary:     op.Summary,
		Description: op.Description,
		Tags:        op.Tags,
		Deprecated:  op.Deprecated,
		Audience:    audience,
	})
}

// MountExplore registers GET /explore on r. It renders one fixed compact Markdown digest of
// every registered operation regardless of audience, grouped by tag — for a Doc whose
// registrations are already all one audience (e.g. clusteragentapi's cluster-agent-only Doc),
// where there is nothing to filter.
func (d *Doc) MountExplore(r chi.Router) {
	d.MountExploreAudience(r, "/explore", "")
}

// MountExploreAudience registers GET path on r, rendering the digest for one audience only —
// an operation registered under a different audience never appears. audience="" renders every
// operation (MountExplore's behavior). Used to give a Doc whose registrations mix audiences
// (e.g. quota-service: agent-facing platform-experiment reads alongside coordinator-only
// lifecycle admin) one /explore per audience from the same registrations, no second source of
// truth.
func (d *Doc) MountExploreAudience(r chi.Router, path string, audience Audience) {
	r.Get(path, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(d.render(audience)))
	})
}

// MountRules registers GET path on r, serving the preamble alone: the rules without the operations.
func (d *Doc) MountRules(r chi.Router, path string) {
	r.Get(path, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(d.preamble))
	})
}

func sectionOf(op opMeta) string {
	if len(op.Tags) > 0 {
		return op.Tags[0]
	}
	return "other"
}

// render renders every registered operation whose Audience matches, or every operation when
// audience is "".
func (d *Doc) render(audience Audience) string {
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
		if audience != "" && op.Audience != audience {
			continue
		}
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

// PlatformRules is the single authority for the rules every agent works under, whatever client it
// uses: the preamble of /explore and all of /explore/rules, which the experimentator splices into
// its system prompt. It names no endpoint, so a rule changed here reaches every agent as is.
const PlatformRules = `## platform rules (read first)

- Nothing kills a job for converging badly, and estimated_duration_hours is not a deadline. Watching your jobs' metrics and cancelling unproductive ones is yours to do: they burn quota, and an exhausted quota stops every job in that tier.
- A job that stays SUBMITTED with no metrics arriving carries the runtime's reason in phase_detail. One whose reason cannot heal is evicted and refunded.
- A RUNNING job that stops reporting metrics is evicted as silent. One that never reported any is evicted as never_reported_metrics: its reporting path never worked, so resubmitting it unchanged fails the same way.
- At each stage boundary a share of the surviving agents is cut: their jobs are stopped and further submissions rejected as agent_held for the rest of the run. You survive on your best value on any one ranking metric. Cuts fire only while the surviving roster is at least 5 agents.
- A stage may set max_job_hours (0 or absent means unlimited). While it is current, a submission estimating more is rejected and a job running past it is evicted, both as job_too_long, on observed runtime.
- Metric values are never validated. Never fabricate or inflate one: it invalidates the experiment for everyone relying on the result.
- Report any metric key a result needs. An undeclared key is stored and shown but never ranked. A declared metric's role is ranking (counts for cuts and standings, the default), constraint (must satisfy a bound or the job is excluded from standings) or attribute (shown, never ranked).
- File a real summary on every COMPLETED job. While one is owed your queued jobs are held, not_admitted_reason summary_gate, and start once it is filed.
- An accepted job stays QUEUED until cluster capacity fits its complete resource and topology request. not_admitted_reason carries the scheduler's current explanation, awaiting_admission until its first look. Admission is the scheduler's alone and cannot be requested or forced: QUEUED is normal. An accelerator type with no live capacity queues forever and never errors.
`
