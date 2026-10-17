package db

import (
	"context"
	"slices"
	"testing"
)

// The first ask for an org's repository records it as wanted and unprovisioned, asking again
// changes nothing, a failed provisioning leaves it wanted, and a done one is provisioned for good
// and no longer to provision.
func TestAnOrgsRepositoryIsWantedOnceAndProvisionedForGood(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	toProvision := func() []string {
		t.Helper()
		orgs, err := w.exps.OrgRegistriesToProvision(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return orgs
	}
	for range 2 {
		if provisioned, err := w.exps.AskOrgRegistry(ctx, w.orgA); err != nil || provisioned {
			t.Fatalf("ask: provisioned %v, %v, want not yet", provisioned, err)
		}
	}
	if pending := toProvision(); !slices.Contains(pending, w.orgA) || slices.Contains(pending, w.orgB) {
		t.Fatalf("to provision %v, want org A and not org B, which never asked", pending)
	}
	if err := w.exps.RecordOrgRegistry(ctx, w.orgA, "iam: not known yet"); err != nil {
		t.Fatal(err)
	}
	if provisioned, err := w.exps.AskOrgRegistry(ctx, w.orgA); err != nil || provisioned || !slices.Contains(toProvision(), w.orgA) {
		t.Fatalf("after a failure: provisioned %v, %v, want it still to provision", provisioned, err)
	}
	if err := w.exps.RecordOrgRegistry(ctx, w.orgA, ""); err != nil {
		t.Fatal(err)
	}
	if provisioned, err := w.exps.AskOrgRegistry(ctx, w.orgA); err != nil || !provisioned || slices.Contains(toProvision(), w.orgA) {
		t.Fatalf("after provisioning: provisioned %v, %v, want it done", provisioned, err)
	}
}
