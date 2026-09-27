package domain

import (
	"testing"
	"time"
)

func TestEffectiveRoleDefaultsToRanking(t *testing.T) {
	d := MetricDefinition{Key: "loss", Direction: "minimize"}
	if got := d.EffectiveRole(); got != MetricRoleRanking {
		t.Fatalf("empty role: got %q, want %q", got, MetricRoleRanking)
	}
}

func TestValidateMetricDefinitions(t *testing.T) {
	bound := 0.99
	cases := []struct {
		name    string
		defs    []MetricDefinition
		wantErr bool
	}{
		{"empty ok", nil, false},
		{"ranking ok", []MetricDefinition{{Key: "loss", Direction: "minimize"}}, false},
		{"explicit ranking ok", []MetricDefinition{{Key: "loss", Direction: "minimize", Role: MetricRoleRanking}}, false},
		{"constraint with bound ok", []MetricDefinition{{Key: "pcc", Direction: "maximize", Role: MetricRoleConstraint, Bound: &bound}}, false},
		{"attribute ok", []MetricDefinition{{Key: "precision_class", Direction: "maximize", Role: MetricRoleAttribute}}, false},
		{"missing key", []MetricDefinition{{Direction: "minimize"}}, true},
		{"bad direction", []MetricDefinition{{Key: "loss", Direction: "up"}}, true},
		{"bad role", []MetricDefinition{{Key: "loss", Direction: "minimize", Role: "nonsense"}}, true},
		{"duplicate key", []MetricDefinition{{Key: "loss", Direction: "minimize"}, {Key: "loss", Direction: "maximize"}}, true},
		{"constraint missing bound", []MetricDefinition{{Key: "pcc", Direction: "maximize", Role: MetricRoleConstraint}}, true},
		{"bound on non-constraint", []MetricDefinition{{Key: "loss", Direction: "minimize", Bound: &bound}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMetricDefinitions(tc.defs)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateMetricDefinitions(%v) = %v, wantErr %v", tc.defs, err, tc.wantErr)
			}
		})
	}
}

// A proposed draft read `ends_at` a week out: the moment its proposal lapses, under the name of
// a run's end.
func TestTheMomentThePlatformClosesItIsNamedForWhatEndsThen(t *testing.T) {
	at, proposed := time.Now(), time.Now()
	cases := []struct {
		name string
		pe   PlatformExperiment
		got  func(PlatformExperiment) time.Time
	}{
		{"a run", PlatformExperiment{Status: PlatformExpOpen}, func(p PlatformExperiment) time.Time { return p.EndsAt }},
		{"an accepted draft", PlatformExperiment{Status: PlatformExpRunning, Goal: "g", StartsAt: at, ProposedAt: &proposed}, func(p PlatformExperiment) time.Time { return p.EndsAt }},
		{"a draft being planned", PlatformExperiment{Status: PlatformExpDraft, Goal: "g"}, func(p PlatformExperiment) time.Time { return p.PlanningEndsAt }},
		{"a proposed draft", PlatformExperiment{Status: PlatformExpDraft, Goal: "g", ProposedAt: &proposed}, func(p PlatformExperiment) time.Time { return p.ProposalExpiresAt }},
		{"a draft closed with its proposal", PlatformExperiment{Status: PlatformExpClosed, Goal: "g", ProposedAt: &proposed}, func(p PlatformExperiment) time.Time { return p.ProposalExpiresAt }},
	}
	for _, c := range cases {
		c.pe.SetClosesAt(at)
		named := 0
		for _, moment := range []time.Time{c.pe.EndsAt, c.pe.PlanningEndsAt, c.pe.ProposalExpiresAt} {
			if !moment.IsZero() {
				named++
			}
		}
		if !c.got(c.pe).Equal(at) || named != 1 || !c.pe.ClosesAt().Equal(at) {
			t.Errorf("%s: got = ends %v, planning ends %v, proposal expires %v, want the moment under one name", c.name, c.pe.EndsAt, c.pe.PlanningEndsAt, c.pe.ProposalExpiresAt)
		}
	}
}

func TestNoRequestAsksForAPlanner(t *testing.T) {
	if _, err := ParseSignupRole(string(SignupRolePlanner)); err == nil {
		t.Fatal("role planner was taken from a request: the platform alone signs a planner up")
	}
	if (AgentSignup{Role: SignupRolePlanner, Flavor: PlannerFlavor}).MeasuresBaseline() {
		t.Fatal("a planner measures no baseline")
	}
}
