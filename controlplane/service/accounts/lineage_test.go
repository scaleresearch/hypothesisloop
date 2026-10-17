package accounts

import (
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestLineageFollowsMintedByThroughAnyDepth(t *testing.T) {
	keys := []*domain.APIKey{
		{ID: "root"}, {ID: "child", MintedBy: "root"}, {ID: "grandchild", MintedBy: "child"},
		{ID: "other"}, {ID: "others-child", MintedBy: "other"},
	}
	for root, want := range map[string]string{"root": "root,child,grandchild", "child": "child,grandchild", "grandchild": "grandchild", "other": "other,others-child", "missing": ""} {
		ids := []string{}
		for _, k := range lineage(keys, root) {
			ids = append(ids, k.ID)
		}
		if got := strings.Join(ids, ","); got != want {
			t.Errorf("lineage of %s: got = %q, want = %q", root, got, want)
		}
	}
}
