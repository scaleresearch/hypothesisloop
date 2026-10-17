package gitrefs

import "testing"

func TestAnAgentsNameIsOneSegmentOfABranch(t *testing.T) {
	for name, want := range map[string]string{"Researcher 2": "researcher-2", "  Ada / Lovelace!! ": "ada-lovelace", "baseline_1": "baseline-1", "***": ""} {
		if got := AgentBranchName(name); got != want {
			t.Errorf("AgentBranchName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestABranchIsUnderAPrefixOnlyWithANameAfterItAndNoWhitespace(t *testing.T) {
	if BranchPrefix("pe-1") != "hl/pe-1/" || BaselineBranch("pe-1") != "hl/pe-1/baseline" {
		t.Fatalf("prefix %q, baseline %q", BranchPrefix("pe-1"), BaselineBranch("pe-1"))
	}
	for branch, want := range map[string]bool{
		"hl/pe-1/ada/idea":   true,
		"hl/pe-1/baseline":   true,
		"hl/pe-1/":           false,
		"hl/pe-1":            false,
		"hl/pe-12/ada/idea":  false,
		"hl/pe-2/ada/idea":   false,
		"main":               false,
		"hl/pe-1/ada idea":   false,
		"hl/pe-1/ada\tidea":  false,
		"hl/pe-1/ada\nidea":  false,
		"x/hl/pe-1/ada/idea": false,
	} {
		if got := IsExperimentBranch("pe-1", branch); got != want {
			t.Errorf("IsExperimentBranch(pe-1, %q) = %v, want %v", branch, got, want)
		}
	}
}

func TestABaselineAgentRunsOnTheBaselineBranchOrOneBesideIt(t *testing.T) {
	for branch, want := range map[string]bool{
		"hl/pe-1/baseline":       true,
		"hl/pe-1/baseline-try":   true,
		"hl/pe-1/baseline-":      false,
		"hl/pe-1/baseline-a/b":   false,
		"hl/pe-1/baseline-a b":   false,
		"hl/pe-1/baselinex":      false,
		"hl/pe-1/ada/idea":       false,
		"hl/pe-2/baseline":       false,
		"hl/pe-1/baseline/more":  false,
		"hl/pe-1/baseline-\tone": false,
	} {
		if got := IsBaselineAgentBranch("pe-1", branch); got != want {
			t.Errorf("IsBaselineAgentBranch(pe-1, %q) = %v, want %v", branch, got, want)
		}
	}
}

func TestASlugIsCutToItsLimitWithNoDashAtEitherEnd(t *testing.T) {
	for _, c := range []struct {
		text  string
		limit int
		want  string
	}{
		{"Warmup helps a lot", 0, "warmup-helps-a-lot"},
		{"Warmup helps a lot", 7, "warmup"},
		{"Warmup helps a lot", 6, "warmup"},
		{"Warmup helps a lot", 9, "warmup-he"},
		{"  --Ünïcode & More!  ", 0, "n-code-more"},
		{"", 5, ""},
		{"!!!", 0, ""},
	} {
		if got := Slug(c.text, c.limit); got != c.want {
			t.Errorf("Slug(%q, %d) = %q, want %q", c.text, c.limit, got, c.want)
		}
	}
}

func TestAnAgentWhoseNameMakesNoBranchOrTheBaselinesHasNoPrefix(t *testing.T) {
	for name, want := range map[string]string{
		"Researcher 2": "hl/pe-1/researcher-2/",
		" Ada!":        "hl/pe-1/ada/",
		"***":          "",
		"Baseline":     "",
		"baseline 1":   "",
		"Baselines":    "hl/pe-1/baselines/",
	} {
		if got := AgentBranchPrefix("pe-1", name); got != want {
			t.Errorf("AgentBranchPrefix(pe-1, %q) = %q, want %q", name, got, want)
		}
		if want != "" && !IsExperimentBranch("pe-1", want+"idea") {
			t.Errorf("a branch under %q is no branch of the experiment", want)
		}
	}
}
