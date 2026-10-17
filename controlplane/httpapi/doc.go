// Package httpapi serves the API. Each operation is registered once, and from that registration
// come the OpenAPI document, the /explore digest per audience, and the check in front of every
// handler. An operation cannot be registered without an Audience, which is its authorization
// class, so the digest an agent reads never lists an operation its token is refused on.
//
// Every error is answered as the API's one error body, platformapi.ErrorModel (see init).
package httpapi

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

	"github.com/scaleresearch/hypothesisloop/contract/platformapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

func init() {
	// Every huma.ErrorXXX and every error huma answers itself is the one error body. A value the
	// caller sent is never repeated in it: a detail's location names the field, and its message
	// says what is wrong with it.
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		problems := make([]platformapi.Problem, 0, len(errs))
		parts := make([]string, 0, len(errs))
		for _, e := range errs {
			var detailer huma.ErrorDetailer
			switch {
			case e == nil:
				continue
			case errors.As(e, &detailer):
				detail := detailer.ErrorDetail()
				problems = append(problems, platformapi.Problem{Message: detail.Message, Field: detail.Location})
				if detail.Location != "" {
					parts = append(parts, detail.Location+": "+detail.Message)
				} else {
					parts = append(parts, detail.Message)
				}
			default:
				parts = append(parts, e.Error())
			}
		}
		message := strings.Join(parts, "; ")
		if msg != "" && message != "" {
			message = msg + ": " + message
		} else if msg != "" {
			message = msg
		}
		refused := refusal.OfStatus(status, message)
		for i := range problems {
			problems[i].Reason = refused.Reason
		}
		if len(problems) > 0 && refused.Reason != refusal.ReasonInternal {
			refused.Problems = problems
		}
		return refused
	}
	// A failure answered here is one no handler logged (auth.Internal logs its own and is answered
	// as it is), such as a raw error a handler returned: it is logged once, with its request.
	huma.NewErrorWithContext = func(ctx huma.Context, status int, msg string, errs ...error) huma.StatusError {
		refused := huma.NewError(status, msg, errs...)
		if logger, ok := ctx.Context().Value(loggerKey{}).(*zap.Logger); ok && refused.GetStatus() >= http.StatusInternalServerError {
			route := ""
			if op := ctx.Operation(); op != nil {
				route = op.Path
			}
			logger.Error("request failed", zap.String("method", ctx.Method()), zap.String("route", route), zap.String("message", msg), zap.Errors("errors", errs))
		}
		return refused
	}
}

// loggerKey holds the logger of the Doc an operation is registered on, which logs a failure huma
// answers.
type loggerKey struct{}

func validAudience(a auth.Audience) bool {
	switch a {
	case auth.AudienceAgent, auth.AudienceJob, auth.AudienceCoordinator, auth.AudienceInternal, auth.AudienceStaff:
		return true
	default:
		return false
	}
}

// shownTo reports whether an operation of this audience belongs in the digest for reader.
func shownTo(a, reader auth.Audience) bool {
	return reader == "" || a == reader || (reader == auth.AudienceAgent && a == auth.AudienceJob)
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
	Audience                           auth.Audience
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
func NewDoc(r chi.Router, title, version, preamble string, access *auth.Access, logger *zap.Logger) *Doc {
	if access == nil || logger == nil {
		panic("httpapi.NewDoc: an access check and a logger are required")
	}
	// A client reads an array the document requires as never null, so no answer may send a nil slice in one.
	huma.DefaultArrayNullable = false
	cfg := huma.DefaultConfig(title, version)
	// No $schema property and Link header on each answer: a client's types come from this document.
	cfg.CreateHooks = nil
	aliasSchemas(cfg.Components.Schemas)
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		bearerScheme: {Type: "http", Scheme: "bearer"},
	}
	// A filter the server doesn't know is a filter that wasn't applied, and returning unfiltered
	// data for it is indistinguishable from a correct answer — a caller filtering by a mistyped or
	// imagined param silently reads the whole table and believes it. 400 instead, naming the param.
	cfg.RejectUnknownQueryParameters = true
	api := humachi.New(r, cfg)
	api.UseMiddleware(func(ctx huma.Context, next func(huma.Context)) {
		next(huma.WithValue(ctx, loggerKey{}, logger))
	})
	return &Doc{API: api, title: title, preamble: preamble, logger: logger, Access: access}
}

// afterCloseKey marks an operation a coordinator token may still call once its platform
// experiment has closed.
const afterCloseKey = "hypothesisloop.after_close"

// AfterClose marks a write a coordinator token may still make once its platform experiment has
// closed. Every read is open to it without the mark.
func afterClose(op huma.Operation) huma.Operation {
	op.Metadata = map[string]any{afterCloseKey: true}
	return op
}

// readKey marks an operation that reads through a method other than GET.
const readKey = "hypothesisloop.read"

// Read marks a POST that reads and writes nothing, as a query whose text does not fit a URL.
// Every GET is a read without the mark.
func read(op huma.Operation) huma.Operation {
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
func (d *Doc) authorize(audience auth.Audience) func(huma.Context, func(huma.Context)) {
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
				fmt.Sprintf("%s token may not call %s %s", withArticle(string(principal.Kind)), op.Method, op.Path))
			return
		}
		if principal.PlatformExperimentClosed && !allowedAfterClose(op) {
			_ = huma.WriteErr(d.API, ctx, http.StatusForbidden,
				fmt.Sprintf("platform experiment %s is closed, and its coordinator token now only reads and files the summary", principal.PlatformExperimentID))
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
func register[I, O any](d *Doc, audience auth.Audience, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	if !validAudience(audience) {
		panic(fmt.Sprintf("httpapi.register %s %s: invalid audience %q", op.Method, op.Path, audience))
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
// registrations are already all one audience (e.g. clusterexchange's cluster-agent-only Doc),
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
func (d *Doc) MountExploreAudience(r chi.Router, path string, audience auth.Audience) {
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
func (d *Doc) render(audience auth.Audience) string {
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
		if !shownTo(op.Audience, audience) {
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
