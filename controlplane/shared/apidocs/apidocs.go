// Package apidocs turns each service's Huma-registered operations into a live,
// self-documenting API surface: an OpenAPI 3.1 document (served by Huma at
// /openapi.json) plus a compact, LLM-friendly Markdown digest served at
// /explore. Agents should fetch /explore at startup instead of reading a
// hand-maintained doc that can drift out of date.
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
	// AudienceAgent: research agents (the Python experimentator) — the surface fetched into an
	// agent's own system prompt at startup, so it must contain nothing an agent shouldn't call.
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

// PlatformRules is the cross-cutting rules preamble agents need regardless of which endpoint
// they hit, embedded in the quota-service /explore (the first service they talk to). It is the
// single authority for these rules — the experimentator's system prompt defers to it rather than
// restating them, so a rule changed here reaches every agent without a prompt edit.
const PlatformRules = `## platform rules (read first)

- No quality eviction: nothing kills a job for converging badly, and estimated_duration_hours is not a deadline. Monitoring runs (GET /experiments/{id}/metrics) and stopping unproductive ones (POST /experiments/{id}/cancel) is yours to do — they burn quota, and quota exhaustion stops every job in that tier.
- If a submitted job stays SUBMITTED/ADMITTED with no metrics arriving, check GET /experiments/{id} for phase_detail before assuming it's just slow to start — a reason there (e.g. an unpullable image) explains why, and a never-self-heals reason evicts and refunds you well before the generic stuck-pending timeout would.
- Silent-eviction: a job that stops reporting metrics while still RUNNING is killed.
- Stage ladder: at each stage boundary a share of the surviving agents is cut — jobs stopped, further submissions rejected 422 agent_held for the rest of the run. You survive on your best value on any one metric; your rank is never visible. GET /platform-experiments/{id}/stages.
- Stage job-length limit: a stage may set max_job_hours (0/absent = unlimited). While it is current, a submission estimating more is rejected job_too_long and a job running past it is evicted job_too_long — measured against observed runtime, so understating estimated_duration_hours does not get you past it. The limit enforced is always the current stage's, which changes as the ladder advances.
- Metric values are never validated server-side. Never fabricate or inflate one — it invalidates the experiment for everyone relying on the result.
- Report any metric key a result needs, not only the platform experiment's declared ones — an undeclared key is still stored and shown on the experiment, it just isn't ranked. A declared metric's role decides what it's for: ranking (counts for cuts/standings, the default), constraint (must satisfy a bound or the job is excluded from standings), attribute (shown, never ranked, never gates eligibility).
- File a real summary (POST /experiments/{id}/summary) after every COMPLETED job, before your next submission.
- An accepted job stays QUEUED until cluster capacity fits its complete resource and topology request — check /resource-catalog/capacity before submitting; while queued, not_admitted_reason carries the scheduler's current explanation. Admission is scheduler-only: there is no endpoint to request or force it, and QUEUED is normal, not stuck.
- Stage cuts only fire once a stage's surviving roster is at least 5 agents; below that, evict_pct on a stage is inert and the ladder is purely a job-length schedule.
`
