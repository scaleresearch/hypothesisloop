package domain

import (
	"testing"
	"time"
)

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
		{"reference ok", []MetricDefinition{{Key: "other_ratio", Direction: "maximize", Role: MetricRoleReference}}, false},
		{"bound on reference", []MetricDefinition{{Key: "other_ratio", Direction: "maximize", Role: MetricRoleReference, Bound: &bound}}, true},
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
// a run's end. An open one read the end of a run that had not started.
func TestTheMomentThePlatformClosesItIsNamedForWhatEndsThen(t *testing.T) {
	at, proposed := time.Now(), time.Now()
	cases := []struct {
		name string
		pe   PlatformExperiment
		got  func(PlatformExperiment) time.Time
	}{
		{"an open one", PlatformExperiment{Status: PlatformExpOpen}, func(p PlatformExperiment) time.Time { return p.BaselineDueAt }},
		{"an accepted draft waiting for its baseline", PlatformExperiment{Status: PlatformExpOpen, Goal: "g", StartsAt: at, DurationHours: 1, ProposedAt: &proposed}, func(p PlatformExperiment) time.Time { return p.BaselineDueAt }},
		{"a run", PlatformExperiment{Status: PlatformExpRunning, StartedAt: at}, func(p PlatformExperiment) time.Time { return p.EndsAt }},
		{"an accepted draft that runs", PlatformExperiment{Status: PlatformExpRunning, Goal: "g", StartsAt: at, StartedAt: at, ProposedAt: &proposed}, func(p PlatformExperiment) time.Time { return p.EndsAt }},
		{"a run that closed", PlatformExperiment{Status: PlatformExpClosed, StartedAt: at}, func(p PlatformExperiment) time.Time { return p.EndsAt }},
		{"one closed before it ran", PlatformExperiment{Status: PlatformExpClosed}, func(p PlatformExperiment) time.Time { return p.BaselineDueAt }},
		{"a draft being planned", PlatformExperiment{Status: PlatformExpDraft, Goal: "g"}, func(p PlatformExperiment) time.Time { return p.PlanningEndsAt }},
		{"a proposed draft", PlatformExperiment{Status: PlatformExpDraft, Goal: "g", ProposedAt: &proposed}, func(p PlatformExperiment) time.Time { return p.ProposalExpiresAt }},
		{"a draft closed with its proposal", PlatformExperiment{Status: PlatformExpClosed, Goal: "g", ProposedAt: &proposed}, func(p PlatformExperiment) time.Time { return p.ProposalExpiresAt }},
	}
	for _, c := range cases {
		c.pe.SetClosesAt(at)
		named := 0
		for _, moment := range []time.Time{c.pe.EndsAt, c.pe.AgentsDueAt, c.pe.BaselineDueAt, c.pe.PlanningEndsAt, c.pe.ProposalExpiresAt} {
			if moment.Equal(at) {
				named++
			}
		}
		if !c.got(c.pe).Equal(at) || named != 1 || !c.pe.ClosesAt().Equal(at) {
			t.Errorf("%s: got = ends %v, baseline due %v, planning ends %v, proposal expires %v, want the moment under one name", c.name, c.pe.EndsAt, c.pe.BaselineDueAt, c.pe.PlanningEndsAt, c.pe.ProposalExpiresAt)
		}
	}
}

// The owner stated 20 minutes of research and the baseline got 10 of them: the run's duration
// was counted from the accept, so a baseline that took long left nothing to research in.
func TestTheRunEndsItsDurationAfterItStartsWhateverTheBaselineTook(t *testing.T) {
	accepted := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	open := PlatformExperiment{Status: PlatformExpOpen, StartsAt: accepted, DurationHours: 1.0 / 3}
	open.SetClosesAt(accepted.Add(45 * time.Minute))
	if !open.EndsAt.IsZero() || !open.BaselineDueAt.Equal(accepted.Add(45*time.Minute)) {
		t.Errorf("open: got = ends %v, baseline due %v, want no end before the run starts and the baseline due at the moment stored", open.EndsAt, open.BaselineDueAt)
	}
	started := accepted.Add(40 * time.Minute)
	waiting := PlatformExperiment{Status: PlatformExpRunning, StartsAt: started, DurationHours: 1.0 / 3}
	waiting.SetClosesAt(started.Add(5 * time.Minute))
	if !waiting.EndsAt.Equal(started.Add(20*time.Minute)) || !waiting.AgentsDueAt.Equal(started.Add(5*time.Minute)) {
		t.Errorf("waiting for a researcher: got = ends %v, agents due %v, want the end 20 minutes after the run started", waiting.EndsAt, waiting.AgentsDueAt)
	}
	if got := ClockStart(time.Time{}, accepted); !got.Equal(accepted) {
		t.Errorf("no starts_at stated: got = %v, want the moment it opens", got)
	}
}

func TestNoRequestAsksForAPlanner(t *testing.T) {
	if _, err := ParseSignupRole(string(SignupRolePlanner)); err == nil {
		t.Fatal("role planner was taken from a request: the platform alone signs a planner up")
	}
}
