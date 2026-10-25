package experimentspec_test

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/contract/experimentspec"
	"github.com/scaleresearch/hypothesisloop/contract/experimentspec/templatetest"
)

// A repository made from the experiment template holds its experiment file with a placeholder in
// each required value, budget_accelerator_hours among them, which no spec parses. It counts as
// unfilled until each is filled in.
func TestTheTemplateExperimentFileIsUnfilledUntilEachPlaceholderIsFilled(t *testing.T) {
	template := []byte(templatetest.Read(experimentspec.ExperimentFile))
	if n := experimentspec.Unfilled(template); n != 7 {
		t.Fatalf("the template's experiment file: %d placeholders, want 7", n)
	}
	if _, err := experimentspec.Parse(template); err == nil {
		t.Fatal("the template's experiment file parsed with its placeholders in")
	}
	if n := experimentspec.Unfilled([]byte(templatetest.FillExperiment("sweep", "val_accuracy"))); n != 0 {
		t.Fatalf("filled in: %d placeholders left, want none", n)
	}
}
