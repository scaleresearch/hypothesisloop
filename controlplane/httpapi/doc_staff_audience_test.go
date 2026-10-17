package httpapi

import (
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
)

func TestTheStaffAudienceAdmitsStaffAndNobodyElse(t *testing.T) {
	for _, kind := range []auth.Kind{auth.KindUser, auth.KindKey, auth.KindAgent, auth.KindCoordinator, auth.KindJob, auth.KindRuntime, auth.KindBuild, auth.KindStaff} {
		p := auth.Principal{Kind: kind, ClusterName: "cluster-a"}
		if got, want := auth.AudienceStaff.Admits(p, "cluster-a"), kind == auth.KindStaff; got != want {
			t.Errorf("a %s token on a staff operation: admitted = %v, want = %v", kind, got, want)
		}
	}
	if !validAudience(auth.AudienceStaff) {
		t.Error("the staff audience is not one an operation may be registered under")
	}
}
