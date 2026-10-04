package environments

import (
	"context"
	"testing"

	"github.com/moby/buildkit/client/llb"
)

func TestEveryFrontendInputIsADefinitionTheDaemonAccepts(t *testing.T) {
	inputs, err := frontendInputs(context.Background(), "ARG BASE\nFROM ${BASE}\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dockerfile", "context"} {
		def, ok := inputs[name]
		if !ok {
			t.Fatalf("no %s input", name)
		}
		if _, err := llb.NewDefinitionOp(def); err != nil {
			t.Errorf("the %s input: %v", name, err)
		}
	}
}
