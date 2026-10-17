package experimentspec

import "testing"

const valid = `
name: tune the model
description: beat the baseline in the README
budget_accelerator_hours: 40
max_agents: 6
report_interval_seconds: 30
duration_hours: 400
metrics:
  - key: val_accuracy
    direction: maximize
stages:
  - length_pct: 100
    evict_pct: 0
`

func TestASpecIsParsedByTheKeysOfTheCreateBody(t *testing.T) {
	spec, err := Parse([]byte(valid + "project_id: prj_1\nrepository: acme/model\nbranch: main\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.Name != "tune the model" || spec.ReportIntervalSeconds != 30 || len(spec.Metrics) != 1 || spec.Metrics[0].Key != "val_accuracy" ||
		len(spec.Stages) != 1 || spec.Stages[0].LengthPct != 100 || spec.BudgetAcceleratorHours != 40 || spec.MaxAgents != 6 || spec.DurationHours != 400 {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Placement != (Placement{ProjectID: "prj_1", Repository: "acme/model", Branch: "main"}) {
		t.Fatalf("placement = %+v", spec.Placement)
	}
	if err := spec.Spec.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for name, file := range map[string]string{
		"a repository account":          valid + "repository_account: acme\n",
		"what it continues":             valid + "continues: pex_1\n",
		"when it starts":                valid + "starts_at: 2026-01-01T00:00:00Z\n",
		"from_repo":                     valid + "from_repo: true\n",
		"a key the body does not have":  valid + "budget_acelerator_hours: 4\n",
		"a list":                        "- name: a\n",
		"not yaml":                      "name: [unclosed\n",
		"a value of the wrong type":     "report_interval_seconds: often\n",
		"empty":                         "",
		"only a comment":                "# nothing yet\n",
		"a key that is a number":        valid + "7: seven\n",
		"a nested key that is a number": "name: x\nmetrics:\n  - key: a\n    direction: maximize\n    1: one\n",
		"a nested key that is a list":   "name: x\nstages:\n  - ? [a, b]\n    : c\n",
	} {
		if _, err := Parse([]byte(file)); err == nil {
			t.Errorf("%s: the file was accepted", name)
		}
	}
}
