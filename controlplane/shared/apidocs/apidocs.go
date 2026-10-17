// Package apidocs registers each service's operations once and derives from that registration
// the OpenAPI document, the /explore digest per audience, and the check in front of every
// handler. An operation cannot be registered without an Audience, which is its authorization
// class, so the digest an agent reads never lists an operation its token is refused on.
//
// Errors keep the {"error": "..."} envelope the agent harness and the UI parse (see init).
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
	"go.uber.org/zap"

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
	// (create/start/close/summary) and other operator-only actions.
	// Admits a signed-in user, an API key, a coordinator token and staff.
	AudienceCoordinator Audience = "coordinator"
	// AudienceInternal: cluster-agent binaries only (the reconcile/status-push contract) —
	// never fetched by a research agent or the coordinator. Admits a runtime token on the
	// cluster its {name} path parameter names, and staff.
	AudienceInternal Audience = "internal"
	// AudienceStaff: the platform's own operators and nobody else. The audience is the whole
	// rule of such an operation: a decision the platform makes across orgs.
	AudienceStaff Audience = "staff"
)

func validAudience(a Audience) bool {
	switch a {
	case AudienceAgent, AudienceJob, AudienceCoordinator, AudienceInternal, AudienceStaff:
		return true
	default:
		return false
	}
}

// Admits reports whether the audience lets this principal in. clusterName is the operation's
// {name} path parameter, empty on an operation that has none: the cluster of an AudienceInternal
// operation.
func (a Audience) Admits(p auth.Principal, clusterName string) bool {
	switch p.Kind {
	case auth.KindStaff:
		return true
	case auth.KindUser, auth.KindKey, auth.KindCoordinator:
		return a == AudienceAgent || a == AudienceJob || a == AudienceCoordinator
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
	API      huma.API
	title    string
	preamble string
	ops      []opMeta
	logger   *zap.Logger
	// Access is what each handler asks whether its caller may reach the resource it loads.
	Access *auth.Access
}

// New builds a Huma API over an existing chi router (preserving its middleware,
// /metrics, /healthz and mount paths) and returns a Doc. Huma automatically
// exposes /openapi.json, /openapi.yaml and /docs on the same router. The router's requests must
// pass auth.Gate first: an operation reads the principal it put in the context.
func New(r chi.Router, title, version, preamble string, access *auth.Access, logger *zap.Logger) *Doc {
	if access == nil || logger == nil {
		panic("apidocs.New: an access check and a logger are required")
	}
	cfg := huma.DefaultConfig(title, version)
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		bearerScheme: {Type: "http", Scheme: "bearer"},
	}
	// A filter the server doesn't know is a filter that wasn't applied, and returning unfiltered
	// data for it is indistinguishable from a correct answer — a caller filtering by a mistyped or
	// imagined param silently reads the whole table and believes it. 400 instead, naming the param.
	cfg.RejectUnknownQueryParameters = true
	return &Doc{API: humachi.New(r, cfg), title: title, preamble: preamble, logger: logger, Access: access}
}

// afterCloseKey marks an operation a coordinator token may still call once its platform
// experiment has closed.
const afterCloseKey = "hypothesisloop.after_close"

// AfterClose marks a write a coordinator token may still make once its platform experiment has
// closed. Every read is open to it without the mark.
func AfterClose(op huma.Operation) huma.Operation {
	op.Metadata = map[string]any{afterCloseKey: true}
	return op
}

// readKey marks an operation that reads through a method other than GET.
const readKey = "hypothesisloop.read"

// Read marks a POST that reads and writes nothing, as a query whose text does not fit a URL.
// Every GET is a read without the mark.
func Read(op huma.Operation) huma.Operation {
	if op.Metadata == nil {
		op.Metadata = map[string]any{}
	}
	op.Metadata[readKey] = true
	return op
}

func reads(op *huma.Operation) bool {
	return op.Method == http.MethodGet || op.Metadata[readKey] == true
}

// withArticle is word after "a" or "an".
func withArticle(word string) string {
	if word != "" && strings.ContainsRune("aeiou", rune(word[0])) {
		return "an " + word
	}
	return "a " + word
}

func allowedAfterClose(op *huma.Operation) bool {
	return op.Method == http.MethodGet || op.Metadata[afterCloseKey] == true
}

// authorize refuses a caller the audience does not admit, the principal auth.Gate resolved. What
// the principal may reach within the operation is the handler's question (auth.Access).
func (d *Doc) authorize(audience Audience) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		op := ctx.Operation()
		principal, ok := auth.FromContext(ctx.Context())
		if !ok {
			refusal := auth.Internal(d.logger, "an operation was reached without passing auth.Gate", errors.New("no principal"), zap.String("path", op.Path))
			_ = huma.WriteErr(d.API, ctx, refusal.GetStatus(), refusal.Message)
			return
		}
		if !audience.Admits(principal, ctx.Param(clusterNameParam)) {
			_ = huma.WriteErr(d.API, ctx, http.StatusForbidden,
				fmt.Sprintf("forbidden: %s token may not call %s %s", withArticle(string(principal.Kind)), op.Method, op.Path))
			return
		}
		if principal.PlatformExperimentClosed && !allowedAfterClose(op) {
			_ = huma.WriteErr(d.API, ctx, http.StatusForbidden,
				fmt.Sprintf("forbidden: platform experiment %s is closed, and its coordinator token now only reads and files the summary", principal.PlatformExperimentID))
			return
		}
		if reads(op) {
			next(huma.WithContext(ctx, auth.WithRead(ctx.Context(), ctx.Query("platform_experiment_id"))))
			return
		}
		next(ctx)
	}
}

// Register registers a Huma operation and records its metadata for the digest. audience is
// required (see Audience) — Register panics on an invalid one, so a call site cannot add an
// operation without classifying who may call it.
//
// An error the handler returns with no status is the platform's own failure: huma would answer
// it as a 500 carrying its text, so it is answered through auth.Internal here.
func Register[I, O any](d *Doc, audience Audience, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	if !validAudience(audience) {
		panic(fmt.Sprintf("apidocs.Register %s %s: invalid audience %q", op.Method, op.Path, audience))
	}
	op.Middlewares = append(op.Middlewares, d.authorize(audience))
	op.Security = []map[string][]string{{bearerScheme: {}}}
	huma.Register(d.API, op, func(ctx context.Context, in *I) (*O, error) {
		out, err := handler(ctx, in)
		var answered huma.StatusError
		if err != nil && !errors.As(err, &answered) {
			return nil, auth.Internal(d.logger, "handler failed", err, zap.String("operation", op.OperationID))
		}
		return out, err
	})
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
