package domain

import (
	"slices"
	"testing"
)

// The architectures of a cluster are the ones its nodes name, each once and sorted; a node that
// names none adds none.
func TestNodeArchitecturesAreTheOnesItsNodesName(t *testing.T) {
	nodes := map[string]map[string]string{
		"a": {NodeArchitectureLabel: "arm64"},
		"b": {NodeArchitectureLabel: "amd64"},
		"c": {NodeArchitectureLabel: "arm64", NodeSlotLabel: "c"},
		"d": {NodeSlotLabel: "d"},
	}
	if got := NodeArchitectures(nodes); !slices.Equal(got, []string{"amd64", "arm64"}) {
		t.Errorf("architectures = %v, want [amd64 arm64]", got)
	}
	if got := NodeArchitectures(nil); len(got) != 0 {
		t.Errorf("no nodes name %v", got)
	}
}

// A node runs a job only on the architecture of its image.
func TestAJobRunsOnANodeOfItsImagesArchitecture(t *testing.T) {
	exp := &Experiment{ImageArchitecture: "amd64"}
	if !exp.OnImageArchitecture(map[string]string{NodeArchitectureLabel: "amd64"}) {
		t.Error("an amd64 job is refused an amd64 node")
	}
	for _, labels := range []map[string]string{{NodeArchitectureLabel: "arm64"}, {}} {
		if exp.OnImageArchitecture(labels) {
			t.Errorf("an amd64 job runs on a node labelled %v", labels)
		}
	}
}
