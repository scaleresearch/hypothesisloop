package experimentspec

import (
	"fmt"
	"math"
)

// MetricRole classifies what a declared MetricDefinition is for.
type MetricRole string

const (
	// MetricRoleRanking counts for stage cuts and standings, and is the role of a metric that
	// states none.
	MetricRoleRanking MetricRole = "ranking"
	// MetricRoleConstraint must be reported and must satisfy Bound. It is judged per job: a
	// completed job that violates it or never reported it ranks nowhere, and the metric itself is
	// never ranked or cut on.
	MetricRoleConstraint MetricRole = "constraint"
	// MetricRoleAttribute is reported and shown, never ranked or cut on, and never gates
	// standings eligibility: "which kind of result is this" facts that have no single order.
	MetricRoleAttribute MetricRole = "attribute"
	// MetricRoleReference is an outside system the goal compares against, measured under a key
	// of its own: reported and shown beside the standings with the machine it ran on, never
	// ranked, cut on or gating, and not required of the baseline, which says why it left one out.
	MetricRoleReference MetricRole = "reference"
)

// ValidMetricRole reports whether role is one of the declared roles, or empty, which is
// MetricRoleRanking.
func ValidMetricRole(role MetricRole) bool {
	switch role {
	case "", MetricRoleRanking, MetricRoleConstraint, MetricRoleAttribute, MetricRoleReference:
		return true
	default:
		return false
	}
}

// MetricDefinition declares a single metric that jobs of a platform experiment must emit.
// The key is the Prometheus metric name; direction is "maximize" or "minimize". Role decides
// what the declaration is for: only MetricRoleRanking metrics ever rank or cut. Bound is required
// and only meaningful for MetricRoleConstraint, expressed in the same direction as Direction
// (maximize: value must be >= Bound; minimize: value must be <= Bound). TargetVsBaseline, optional
// and only on a ranking metric, is the goal as a signed change against the baseline's value, in
// fractions of its magnitude. Deterministic declares that the same code and data give the same
// value on any machine and in every run.
type MetricDefinition struct {
	Key              string     `json:"key"`
	Direction        string     `json:"direction" enum:"maximize,minimize"`
	Role             MetricRole `json:"role,omitempty" enum:"ranking,constraint,attribute,reference"`
	Bound            *float64   `json:"bound,omitempty"`
	TargetVsBaseline *float64   `json:"target_vs_baseline,omitempty" doc:"Optional, on a ranking metric: the goal as a change against the declared baseline's value, in fractions of its magnitude. 0.5 is 50% above the baseline, -0.3 is 30% below it. Positive for a metric to maximize, negative for one to minimize."`
	Deterministic    bool       `json:"deterministic,omitempty" doc:"Optional: the same code and data give the same value on any machine and in every run, as an exact count or check does and a timing never does. Its values compare across machines and need no repeat."`
}

// EffectiveRole returns d.Role, defaulting empty to MetricRoleRanking.
func (d MetricDefinition) EffectiveRole() MetricRole {
	if d.Role == "" {
		return MetricRoleRanking
	}
	return d.Role
}

// Target is the value TargetVsBaseline sets against baseline: nil without one, or with a baseline
// of 0, which no fraction of its magnitude moves.
func (d MetricDefinition) Target(baseline float64) *float64 {
	if d.TargetVsBaseline == nil || baseline == 0 {
		return nil
	}
	target := baseline + *d.TargetVsBaseline*math.Abs(baseline)
	return &target
}

// Holds reports whether value satisfies the bound of a constraint metric.
func (d MetricDefinition) Holds(value float64) bool {
	if d.Direction == "minimize" {
		return value <= *d.Bound
	}
	return value >= *d.Bound
}

// Score is value turned so that higher is better: itself for a metric maximized, its negation
// for one minimized.
func (d MetricDefinition) Score(value float64) float64 {
	if d.Direction == "minimize" {
		return -value
	}
	return value
}

// Better reports whether a is strictly better than b in the metric's direction.
func (d MetricDefinition) Better(a, b float64) bool { return d.Score(a) > d.Score(b) }

// ValidateMetricDefinitions enforces the metric contract at creation, so nothing downstream
// (stage cuts, standings) needs to defend against a malformed one.
func ValidateMetricDefinitions(defs []MetricDefinition) error {
	seen := make(map[string]bool, len(defs))
	for i, d := range defs {
		if d.Key == "" {
			return fmt.Errorf("metric %d: key is required", i)
		}
		if seen[d.Key] {
			return fmt.Errorf("metric %d: duplicate key %q", i, d.Key)
		}
		seen[d.Key] = true
		if d.Direction != "maximize" && d.Direction != "minimize" {
			return fmt.Errorf("metric %q: direction must be \"maximize\" or \"minimize\", got %q", d.Key, d.Direction)
		}
		if !ValidMetricRole(d.Role) {
			return fmt.Errorf("metric %q: role must be one of ranking, constraint, attribute, reference (or empty), got %q", d.Key, d.Role)
		}
		if d.EffectiveRole() == MetricRoleConstraint && d.Bound == nil {
			return fmt.Errorf("metric %q: constraint role requires bound", d.Key)
		}
		if d.EffectiveRole() != MetricRoleConstraint && d.Bound != nil {
			return fmt.Errorf("metric %q: bound is only meaningful for constraint role", d.Key)
		}
		if err := d.validateTarget(); err != nil {
			return err
		}
	}
	return nil
}

// validateTarget refuses a target that is not a ranking metric's or lies on the wrong side of the
// baseline for its direction, which the run would have reached before it started.
func (d MetricDefinition) validateTarget() error {
	t := d.TargetVsBaseline
	switch {
	case t == nil:
		return nil
	case d.EffectiveRole() != MetricRoleRanking:
		return fmt.Errorf("metric %q: target_vs_baseline is only meaningful for ranking role", d.Key)
	case math.IsNaN(*t) || math.IsInf(*t, 0):
		return fmt.Errorf("metric %q: target_vs_baseline must be a finite number", d.Key)
	case d.Direction == "maximize" && *t <= 0:
		return fmt.Errorf("metric %q: target_vs_baseline %g is not above the baseline, and the metric is maximized: state a positive change, 0.5 for 50%% above it", d.Key, *t)
	case d.Direction == "minimize" && *t >= 0:
		return fmt.Errorf("metric %q: target_vs_baseline %g is not below the baseline, and the metric is minimized: state a negative change, -0.3 for 30%% below it", d.Key, *t)
	}
	return nil
}

// PrimaryRankingMetric is the first declared ranking metric, the primary one by convention.
func PrimaryRankingMetric(defs []MetricDefinition) (MetricDefinition, bool) {
	for _, d := range defs {
		if d.EffectiveRole() == MetricRoleRanking {
			return d, true
		}
	}
	return MetricDefinition{}, false
}

// RequireRankingMetric rejects a metric contract that can never rank anyone. Constraint and
// attribute metrics gate and describe; only a ranking metric orders a field. Without one the
// ladder runs to the end cutting nobody and the results endpoint returns no standings: a run
// that consumed a real budget and answered nothing, silently. Enforced where the contract is
// written (create/update) and never on read: a row predating this rule must still reconcile.
func RequireRankingMetric(defs []MetricDefinition) error {
	if _, ok := PrimaryRankingMetric(defs); ok {
		return nil
	}
	return fmt.Errorf("at least one metric must have role \"ranking\": nothing else can order the field or cut a stage")
}
