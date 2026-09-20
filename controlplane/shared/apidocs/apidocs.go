// Package apidocs turns each service's Huma-registered operations into a live,
// self-documenting API surface: an OpenAPI 3.1 document (served by Huma at
// /openapi.json) plus a compact, LLM-friendly Markdown digest served at
// /explore, so nobody reads a hand-maintained doc that can drift out of date.
//
// Every Register call declares an Audience — required, no default. The audience is the
// operation's authorization class: Register puts a check in front of the handler that resolves
// the caller's token and refuses a kind of caller the audience does not admit. The same
// declaration renders /explore per audience (MountExploreAudience), so a research agent's
// digest cannot contain an operation its token would be refused on.
//
// It also pins the platform's error-response shape to the historical
// {"error": "..."} envelope (see init) so existing consumers — the Python
// research agent and the controlplane/ui dashboard — keep working unchanged
// even though Huma defaults to RFC7807 problem+json.
package apidocs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
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
	// shouldn't call. Admits everyone AudienceCoordinator does, and an agent token.
	AudienceAgent Audience = "agent"
	// AudienceJob: AudienceAgent plus a job token. The one thing a job may say to the platform
	// is a metric, so exactly one operation carries this audience.
	AudienceJob Audience = "job"
	// AudienceCoordinator: the coordinator/operator/dashboard — experiment lifecycle admin
	// (create/start/close/summary), donation administration, and other operator-only actions.
	// Admits a signed-in user, an API key, a coordinator token and staff.
	AudienceCoordinator Audience = "coordinator"
	// AudienceInternal: cluster-agent binaries only (the reconcile/status-push contract) —
	// never fetched by a research agent or the coordinator. Admits a runtime token on the
	// cluster its {name} path parameter names, and staff.
	AudienceInternal Audience = "internal"
)

func validAudience(a Audience) bool {
	switch a {
	case AudienceAgent, AudienceJob, AudienceCoordinator, AudienceInternal:
		return true
	default:
		return false
	}
}

// Admits reports whether the audience lets this principal in. clusterName is the operation's
// {name} path parameter, empty on an operation that has none.
func (a Audience) Admits(p auth.Principal, clusterName string) bool {
	switch p.Kind {
	case auth.KindStaff:
		return true
	case auth.KindUser, auth.KindKey, auth.KindCoordinator:
		return a != AudienceInternal
	case auth.KindAgent:
		return a == AudienceAgent || a == AudienceJob
	case auth.KindJob:
		return a == AudienceJob
	case auth.KindRuntime:
		return a == AudienceInternal && clusterName != "" && clusterName == p.ClusterName
	default:
		return false
	}
}

// shownTo reports whether an operation of this audience belongs in the digest for reader.
func (a Audience) shownTo(reader Audience) bool {
	return reader == "" || a == reader || (reader == AudienceAgent && a == AudienceJob)
}

// Authenticator resolves a request's token. *auth.Authenticator is the one used in service.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (auth.Principal, error)
}

// clusterNameParam is the path parameter an AudienceInternal operation names its cluster with.
const clusterNameParam = "name"

const bearerScheme = "bearer"

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
	API           huma.API
	title         string
	preamble      string
	ops           []opMeta
	authenticator Authenticator
	// Access is what each handler asks whether its caller may reach the resource it loads.
	Access *auth.Access
}

// New builds a Huma API over an existing chi router (preserving its middleware,
// /metrics, /healthz and mount paths) and returns a Doc. Huma automatically
// exposes /openapi.json, /openapi.yaml and /docs on the same router.
func New(r chi.Router, title, version, preamble string, authenticator Authenticator, access *auth.Access) *Doc {
	if authenticator == nil || access == nil {
		panic("apidocs.New: an authenticator and an access check are required")
	}
	cfg := huma.DefaultConfig(title, version)
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		bearerScheme: {Type: "http", Scheme: "bearer"},
	}
	// A filter the server doesn't know is a filter that wasn't applied, and returning unfiltered
	// data for it is indistinguishable from a correct answer — a caller filtering by a mistyped or
	// imagined param silently reads the whole table and believes it. 400 instead, naming the param.
	cfg.RejectUnknownQueryParameters = true
	return &Doc{API: humachi.New(r, cfg), title: title, preamble: preamble, authenticator: authenticator, Access: access}
}

// authorize is the one place a request is let in: it resolves the token, refuses a caller the
// audience does not admit, and hands the handler a context carrying the principal. What the
// principal may reach within the operation is the handler's question (auth.Access).
func (d *Doc) authorize(audience Audience) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		token, err := auth.BearerToken(ctx.Header("Authorization"))
		if err != nil {
			d.refuse(ctx, err)
			return
		}
		principal, err := d.authenticator.Authenticate(ctx.Context(), token)
		if err != nil {
			d.refuse(ctx, err)
			return
		}
		if !audience.Admits(principal, ctx.Param(clusterNameParam)) {
			_ = huma.WriteErr(d.API, ctx, http.StatusForbidden,
				fmt.Sprintf("forbidden: a %s token may not call %s %s", principal.Kind, ctx.Operation().Method, ctx.Operation().Path))
			return
		}
		next(huma.WithContext(ctx, auth.WithPrincipal(ctx.Context(), principal)))
	}
}

func (d *Doc) refuse(ctx huma.Context, err error) {
	var refusal *auth.Error
	if errors.As(err, &refusal) {
		_ = huma.WriteErr(d.API, ctx, refusal.GetStatus(), refusal.Message)
		return
	}
	_ = huma.WriteErr(d.API, ctx, http.StatusInternalServerError, "could not authenticate the request: "+err.Error())
}

// Register registers a Huma operation and records its metadata for the digest. audience is
// required (see Audience) — Register panics on an invalid one, so a call site cannot add an
// operation without classifying who may call it.
func Register[I, O any](d *Doc, audience Audience, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	if !validAudience(audience) {
		panic(fmt.Sprintf("apidocs.Register %s %s: invalid audience %q", op.Method, op.Path, audience))
	}
	op.Middlewares = append(op.Middlewares, d.authorize(audience))
	op.Security = []map[string][]string{{bearerScheme: {}}}
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
		if !op.Audience.shownTo(audience) {
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
