package agenttypes

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestEachBuiltinHasAValidDescription(t *testing.T) {
	all, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.Description == "" {
			t.Errorf("%s has no description", b.ID)
		}
		if err := domain.ValidateAgentTypeDescription(b.Description); err != nil {
			t.Errorf("%s: %v", b.ID, err)
		}
	}
}
