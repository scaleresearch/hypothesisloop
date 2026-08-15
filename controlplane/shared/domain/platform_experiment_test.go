package domain

import "testing"

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
