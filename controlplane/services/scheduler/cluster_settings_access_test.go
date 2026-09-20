package scheduler

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/auth/authtest"
)

type reportedIDsWorkload struct {
	concurrentAdmissionWorkload
	ids map[string]string
}

func (w *reportedIDsWorkload) GetClusterIDs(context.Context) (map[string]string, error) {
	return w.ids, nil
}

// Settings are keyed by the id a cluster reports and a cluster is owned by name, so the check
// goes through the connected cluster that reports the id.
func TestClusterSettingsAreReachedThroughTheClusterThatReportsTheID(t *testing.T) {
	workload := &reportedIDsWorkload{ids: map[string]string{"cluster-a": "uid-a", "cluster-b": "uid-b"}}
	handler := NewHandler(NewService(nil, nil, workload, nil, nil, noopSettler{}, testGraceLimits, "http://metrics.invalid", zap.NewNop()).WithLoop(noopLoop{}))
	access := auth.NewAccess(authtest.Placement{Clusters: map[string]string{"cluster-a": "org-a", "cluster-b": "org-b"}})

	userOfA := auth.Principal{Kind: auth.KindUser, OrgIDs: []string{"org-a"}}
	staff := auth.Principal{Kind: auth.KindStaff}
	for _, tc := range []struct {
		name      string
		caller    auth.Principal
		clusterID string
		admitted  bool
	}{
		{"owner, by the id its cluster reports", userOfA, "uid-a", true},
		{"another org's cluster", userOfA, "uid-b", false},
		{"its own cluster's name used as an id", userOfA, "cluster-a", false},
		{"an id no connected cluster reports", userOfA, "uid-gone", false},
		{"staff, any cluster", staff, "uid-b", true},
		{"staff, an id no connected cluster reports", staff, "uid-gone", true},
	} {
		err := handler.clusterAccess(auth.WithPrincipal(context.Background(), tc.caller), access, tc.clusterID)
		var refusal *auth.Error
		if tc.admitted && err != nil {
			t.Errorf("%s: got = %v, want admitted", tc.name, err)
		}
		if !tc.admitted && (!errors.As(err, &refusal) || refusal.GetStatus() != 404) {
			t.Errorf("%s: got = %v, want 404", tc.name, err)
		}
	}
}
