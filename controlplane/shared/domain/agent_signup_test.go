package domain

import (
	"strings"
	"testing"
)

func TestAGeneratedAgentIDMeetsTheIdentifierRuleAndIsNeverRepeated(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id, err := NewAgentID()
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateIdentifier("id", id); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
		if !strings.HasPrefix(id, "ag-") || seen[id] {
			t.Fatalf("id %q is repeated or has no ag- prefix", id)
		}
		seen[id] = true
	}
}

func TestAgentLaunchValuesAreSafeAsEnvironment(t *testing.T) {
	for _, ok := range [][2]string{{"", ""}, {"generalist", "claude-sonnet-5"}, {"data-search", "claude-opus-5[1m]"}, {"x", "vendor.model:v1_2"}} {
		if err := ValidateAgentLaunch(ok[0], ok[1]); err != nil {
			t.Errorf("flavor %q model %q refused: %v", ok[0], ok[1], err)
		}
	}
	for _, bad := range [][2]string{
		{"Generalist", ""}, {"../etc", ""}, {"a b", ""}, {strings.Repeat("f", 64), ""},
		{"", "model with space"}, {"", "$(id)"}, {"", "-leading"}, {"", strings.Repeat("m", 129)}, {"", "a\nb"},
	} {
		if err := ValidateAgentLaunch(bad[0], bad[1]); err == nil {
			t.Errorf("flavor %q model %q accepted", bad[0], bad[1])
		}
	}
}
