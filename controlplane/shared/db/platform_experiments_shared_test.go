package db

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestSharingAPlatformExperimentIsReadWhereverItIsPlacedAndListed(t *testing.T) {
	pool := eventsTestDB(t)
	accounts, pes := NewAccountsStore(pool), NewPlatformExperimentsStore(pool)
	ctx := context.Background()
	_, orgID := newTestUserHome(t, accounts)
	_, otherOrg := newTestUserHome(t, accounts)
	project := defaultProjectOf(t, accounts, orgID)
	suffix := uuid.New().String()[:8]
	shown, kept := "pe-shown-"+suffix, "pe-kept-"+suffix
	createTestPEIn(t, pool, shown, project.ID)
	createTestPEIn(t, pool, kept, project.ID)

	shared := func(id string) bool {
		t.Helper()
		tenancy, _, err := accounts.PlatformExperimentTenancy(ctx, id)
		if err != nil || tenancy == nil {
			t.Fatalf("tenancy of %s: got = %+v, %v, want it placed", id, tenancy, err)
		}
		pe, err := pes.GetPlatformExperiment(ctx, id)
		if err != nil || pe == nil || pe.Shared != tenancy.Shared {
			t.Fatalf("read of %s: got = %+v, %v, want shared = %v as its tenancy says", id, pe, err, tenancy.Shared)
		}
		return tenancy.Shared
	}
	if shared(shown) || shared(kept) {
		t.Fatal("a platform experiment is shared before staff shared it")
	}
	if found, err := pes.SetPlatformExperimentShared(ctx, shown, true); err != nil || !found {
		t.Fatalf("share: got = %v, %v, want it found", found, err)
	}
	if found, err := pes.SetPlatformExperimentShared(ctx, shown, true); err != nil || !found {
		t.Fatalf("sharing a shared one: got = %v, %v, want it found", found, err)
	}
	if !shared(shown) || shared(kept) {
		t.Fatal("sharing one platform experiment must share it and no other")
	}
	if found, err := pes.SetPlatformExperimentShared(ctx, "pe-none-"+suffix, true); err != nil || found {
		t.Fatalf("sharing one that does not exist: got = %v, %v, want it not found", found, err)
	}

	listed := func(filter PlatformExperimentsFilter) []string {
		t.Helper()
		filter.Search, filter.Limit = suffix, 200
		rows, err := pes.ListPlatformExperiments(ctx, filter)
		if err != nil {
			t.Fatalf("list: got = %v, want = nil", err)
		}
		total, err := pes.CountPlatformExperiments(ctx, filter)
		if err != nil || total != len(rows) {
			t.Fatalf("count: got = %d, %v, want = %d as listed", total, err, len(rows))
		}
		ids := make([]string, len(rows))
		for i, pe := range rows {
			ids[i] = pe.ID
			if pe.Shared != (pe.ID == shown) {
				t.Errorf("listed %s with shared = %v", pe.ID, pe.Shared)
			}
		}
		slices.Sort(ids)
		return ids
	}
	another := domain.TenantScope{OrgIDs: []string{otherOrg}}
	everyOrg := domain.TenantScope{AllOrgs: true}
	if got := listed(PlatformExperimentsFilter{Scope: another}); len(got) != 0 {
		t.Fatalf("another org's own listing: got = %v, want none: a listing is the caller's own", got)
	}
	if got := listed(PlatformExperimentsFilter{Scope: everyOrg, Shared: true}); !slices.Equal(got, []string{shown}) {
		t.Fatalf("the shared ones: got = %v, want = [%s]", got, shown)
	}
	if got := listed(PlatformExperimentsFilter{Scope: another, Shared: true}); len(got) != 0 {
		t.Fatalf("the shared ones within another org's own scope: got = %v, want none", got)
	}
	// What a listing narrowed to the shared one is given: every org, that platform experiment.
	narrowed := domain.TenantScope{AllOrgs: true, PlatformExperimentID: shown}
	if got := listed(PlatformExperimentsFilter{Scope: narrowed}); !slices.Equal(got, []string{shown}) {
		t.Fatalf("a listing narrowed to the shared one: got = %v, want = [%s]", got, shown)
	}
	if ids, err := accounts.PlatformExperimentIDs(ctx, narrowed); err != nil || !slices.Equal(ids, []string{shown}) {
		t.Fatalf("platform experiments inside that scope: got = %v, %v, want = [%s]", ids, err, shown)
	}

	if found, err := pes.SetPlatformExperimentShared(ctx, shown, false); err != nil || !found {
		t.Fatalf("unshare: got = %v, %v, want it found", found, err)
	}
	if shared(shown) {
		t.Fatal("a platform experiment is still shared after staff took it back")
	}
	if got := listed(PlatformExperimentsFilter{Scope: everyOrg, Shared: true}); len(got) != 0 {
		t.Fatalf("the shared ones after the unshare: got = %v, want none", got)
	}
}
