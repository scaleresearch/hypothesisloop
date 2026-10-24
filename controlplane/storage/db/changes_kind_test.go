package db

import (
	"slices"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// A kind the domain emits that changes_kind does not allow fails its insert only on a live stack.
func TestEveryEventKindIsAllowedByTheChangesKindCheck(t *testing.T) {
	allowed := quoted(schemaList(t, "CREATE TABLE changes ", "kind"))
	for _, k := range domain.EventKinds {
		if !slices.Contains(allowed, k.Kind) {
			t.Errorf("event kind %q is not in the changes_kind CHECK of schema.sql", k.Kind)
		}
	}
	for _, kind := range allowed {
		if !slices.ContainsFunc(domain.EventKinds, func(k domain.EventKindDoc) bool { return k.Kind == kind }) {
			t.Errorf("changes_kind allows %q, which is no domain event kind", kind)
		}
	}
}
