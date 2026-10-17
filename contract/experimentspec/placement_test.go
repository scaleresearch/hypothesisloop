package experimentspec

import (
	"strings"
	"testing"
)

func TestAPlacementSaysProjectRepositoryAndBranch(t *testing.T) {
	whole := Placement{ProjectID: "prj_1", Repository: "acme/model", Branch: "main"}
	if err := whole.Validate(); err != nil {
		t.Fatalf("a whole placement: %v", err)
	}
	for name, p := range map[string]Placement{
		"no project":    {Repository: "acme/model", Branch: "main"},
		"no repository": {ProjectID: "prj_1", Branch: "main"},
		"no branch":     {ProjectID: "prj_1", Repository: "acme/model"},
		"nothing":       {},
	} {
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), "project_id, repository and branch are required") {
			t.Errorf("%s: %v, want the three keys named as required", name, err)
		}
	}
	under := whole
	under.Branch = "hl/pe-1/baseline"
	if err := under.Validate(); err == nil || !strings.Contains(err.Error(), "hl/") {
		t.Errorf("a branch of the platform's agents: %v, want it refused", err)
	}
}

func TestABranchAnExperimentStartsFromIsOneGitTakes(t *testing.T) {
	for _, branch := range []string{"main", "dev", "feature/warmup", "release-1.2", "a.b/c_d", "UPPER", "x@y", strings.Repeat("b", 255)} {
		if err := ValidateBranch(branch); err != nil {
			t.Errorf("ValidateBranch(%q) = %v, want it taken", branch, err)
		}
	}
	for _, branch := range []string{
		"", strings.Repeat("b", 256), "hl/pe-1/ada", "hl/x", "@", "-main", ".hidden", "/main", "main/", "main.", "main.lock",
		"a..b", "a//b", "a@{1}", "a/.b", "a b", "a\tb", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", `a\b`, "a\x7fb", "a\x00b",
	} {
		if err := ValidateBranch(branch); err == nil {
			t.Errorf("ValidateBranch(%q) = nil, want it refused", branch)
		}
	}
}

func TestAnExperimentFileStatesItsPlacementBesideTheSpec(t *testing.T) {
	file, err := Parse([]byte(valid + "project_id: prj_1\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if file.ProjectID != "prj_1" || file.Repository != "" || file.Branch != "" {
		t.Fatalf("placement = %+v, want the project alone", file.Placement)
	}
	if err := file.Placement.Validate(); err == nil {
		t.Error("a file with no repository and branch has a whole placement")
	}
	if err := file.Spec.Validate(); err != nil {
		t.Errorf("the spec beside a partial placement: %v", err)
	}
}
