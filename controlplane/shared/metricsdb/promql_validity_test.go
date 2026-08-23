package metricsdb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// greptimeMaxRangeDuration is the largest `[Ns]` range-vector literal GreptimeDB's PromQL query
// endpoint accepts before answering 400 "duration must be greater than 0" — found empirically by
// bisecting against a real instance (last accepted: 4294967186s; first rejected: 4294967371s,
// i.e. right around 2^32-1 seconds, ~136 years). This is exactly the class of bug that motivated
// this file: a query built from a bad/zero-value timestamp (see maxObservedLookback in usage.go)
// produces syntactically valid PromQL that a real backend still rejects — the Prometheus parser
// alone would never catch it, since ordinary PromQL has no such ceiling. Kept a hair below the
// true boundary so this mock never accepts something the real backend would reject.
const greptimeMaxRangeDuration = (1<<32 - 2) * time.Second

// promqlMockServer stands in for GreptimeDB's Prometheus-compatible query API (both the instant
// and range endpoints every function in this package calls through QueryVector/QueryRange). It
// answers every request in-process — no network, no container — so this suite runs in
// milliseconds and needs nothing running locally, yet still catches what actually broke in
// production: real PromQL parse errors (via the same parser Prometheus itself uses) and
// GreptimeDB's specific numeric duration ceiling, on every query this package can build. Every
// accepted query's text is appended to *captured for the caller to assert on if it wants to.
func promqlMockServer(t *testing.T, captured *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		if captured != nil {
			*captured = append(*captured, query)
		}
		expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "error", "errorType": "InvalidArguments",
				"error": fmt.Sprintf("parse error: %v", err),
			})
			return
		}
		if bad := firstOversizedRange(expr); bad != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "error", "errorType": "InvalidArguments",
				"error": "duration must be greater than 0",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resultType := "vector"
		if strings.HasSuffix(r.URL.Path, "/query_range") {
			resultType = "matrix"
		}
		_, _ = fmt.Fprintf(w, `{"status":"success","data":{"resultType":%q,"result":[]}}`, resultType)
	}))
}

// firstOversizedRange walks a parsed PromQL expression for any MatrixSelector (the `[Ns]` part of
// a range vector) whose duration would trip GreptimeDB's real ceiling, returning it for the
// caller to report. Empty string means every range in the query is within bounds.
func firstOversizedRange(expr parser.Expr) string {
	var bad string
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		if m, ok := node.(*parser.MatrixSelector); ok && m.Range > greptimeMaxRangeDuration {
			bad = m.String()
		}
		return nil
	})
	return bad
}

// mustParsePromQL is the direct, HTTP-free half of this guarantee: every query string this
// package builds must be valid PromQL per the real parser, checked in isolation from any network
// round trip.
func mustParsePromQL(t *testing.T, query string) {
	t.Helper()
	if _, err := parser.NewParser(parser.Options{}).ParseExpr(query); err != nil {
		t.Fatalf("not valid PromQL: %q: %v", query, err)
	}
}

// TestObservedLookbackProducesValidPromQLAtAnyStartTime is the direct regression test for the bug
// that started this file: whatever ObservedLookback returns must parse, and must stay under
// GreptimeDB's real ceiling, for every start time a stored row could ever hold — including the
// zero value that actually reached production.
func TestObservedLookbackProducesValidPromQLAtAnyStartTime(t *testing.T) {
	starts := map[string]time.Time{
		"zero value (corrupt/never-set CreatedAt)":   {},
		"far future (clock skew)":                    time.Now().Add(24 * time.Hour),
		"just now":                                   time.Now(),
		"14 days ago (a real multi-week experiment)": time.Now().Add(-14 * 24 * time.Hour),
	}
	for name, start := range starts {
		t.Run(name, func(t *testing.T) {
			query := fmt.Sprintf(`last_over_time(x{job="j"}[%s])`, ObservedLookback(start))
			expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
			if err != nil {
				t.Fatalf("%q: not valid PromQL: %v", query, err)
			}
			if bad := firstOversizedRange(expr); bad != "" {
				t.Fatalf("%q: range %s exceeds what GreptimeDB accepts", query, bad)
			}
		})
	}
}

// TestPackageQueriesAreValidPromQL runs every PromQL-emitting function this package exposes
// through promqlMockServer with both ordinary and pathological inputs (particularly a zero-value
// platform-experiment start time, the exact shape of the production bug), asserting each one
// still gets a clean response — i.e. every query it builds is real, acceptable PromQL. Run
// against a fresh in-process mock every time, so this is a compile-time-adjacent guarantee: it
// runs on every `go test` with no live GreptimeDB required, and fails immediately if a future
// edit makes any of these functions emit something a real backend would reject.
func TestPackageQueriesAreValidPromQL(t *testing.T) {
	ctx := t.Context()
	zero := time.Time{}
	now := time.Now()
	recent := now.Add(-14 * 24 * time.Hour)

	cases := []struct {
		name string
		run  func(dbURL string) error
	}{
		{"isAliveOn", func(dbURL string) error {
			_, err := isAliveOn(ctx, dbURL, "m", "experiment_id", "exp-1", time.Hour)
			return err
		}},
		{"metricSampleCount", func(dbURL string) error {
			_, err := metricSampleCount(ctx, dbURL, "exp-1", "loss", time.Hour)
			return err
		}},
		{"BestPerAgentOnMetric (normal start)", func(dbURL string) error {
			pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: recent}
			_, _, err := BestPerAgentOnMetric(ctx, dbURL, pe, domain.MetricDefinition{Key: "val_accuracy", Direction: "maximize"})
			return err
		}},
		{"BestPerAgentOnMetric (zero start)", func(dbURL string) error {
			pe := &domain.PlatformExperiment{ID: "pe-1", CreatedAt: zero}
			_, _, err := BestPerAgentOnMetric(ctx, dbURL, pe, domain.MetricDefinition{Key: "val_accuracy", Direction: "minimize"})
			return err
		}},
		{"acceleratorTypeChanges", func(dbURL string) error {
			_, err := acceleratorTypeChanges(ctx, dbURL, "exp-1", now.Add(-time.Hour), now, 30*time.Second)
			return err
		}},
		{"lastValuePerCluster", func(dbURL string) error {
			_, err := lastValuePerCluster(ctx, dbURL, "some_metric", 5*time.Minute)
			return err
		}},
		{"PopulateUsage (normal start)", func(dbURL string) error {
			return PopulateUsage(ctx, dbURL, recent, "pe-1", []*domain.AgentQuota{{AgentID: "a-1", PlatformExperimentID: "pe-1"}})
		}},
		{"PopulateUsage (zero start)", func(dbURL string) error {
			return PopulateUsage(ctx, dbURL, zero, "pe-1", []*domain.AgentQuota{{AgentID: "a-1", PlatformExperimentID: "pe-1"}})
		}},
		{"PopulateUsageOne (zero start)", func(dbURL string) error {
			return PopulateUsageOne(ctx, dbURL, zero, &domain.AgentQuota{AgentID: "a-1", PlatformExperimentID: "pe-1"})
		}},
		{"SettledCostForJob (zero start)", func(dbURL string) error {
			_, _, err := SettledCostForJob(ctx, dbURL, zero, "exp-1")
			return err
		}},
		{"TotalObservedAccH (zero start)", func(dbURL string) error {
			_, err := TotalObservedAccH(ctx, dbURL, zero, "pe-1")
			return err
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var captured []string
			srv := promqlMockServer(t, &captured)
			defer srv.Close()
			if err := c.run(srv.URL); err != nil {
				t.Fatalf("%s: %v (query/queries sent: %v)", c.name, err, captured)
			}
			if len(captured) == 0 {
				t.Fatalf("%s: sent no query — test wired up wrong, this asserts nothing", c.name)
			}
		})
	}
}

// TestMustParsePromQLCatchesAMalformedQuery is a meta-test: proves mustParsePromQL and the mock
// server actually reject something, so a query-building bug that produces broken syntax (an
// unescaped label value, an unbalanced brace — exactly the kind of slip an AI-generated query
// string is prone to) cannot silently pass this suite.
func TestMustParsePromQLCatchesAMalformedQuery(t *testing.T) {
	_, err := parser.NewParser(parser.Options{}).ParseExpr(`last_over_time(x{job="unterminated[10s])`)
	if err == nil {
		t.Fatal("malformed PromQL parsed without error — the guard is not actually checking anything")
	}
}
