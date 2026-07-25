package main

import (
	"reflect"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestOrphanAuxiliaryIDsOnlyDeletesResourcesWithNoDesiredOrActualJob(t *testing.T) {
	desired := map[string]*domain.Experiment{"desired": {ID: "desired"}}
	actual := map[string]bool{"actual": true}
	got := orphanAuxiliaryIDs(desired, actual, []string{"desired", "actual", "orphan"})
	if !reflect.DeepEqual(got, []string{"orphan"}) {
		t.Fatalf("orphanAuxiliaryIDs = %v, want [orphan]", got)
	}
}
