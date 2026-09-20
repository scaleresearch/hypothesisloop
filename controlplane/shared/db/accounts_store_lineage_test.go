package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestRevokingAKeyRevokesEveryKeyMintedFromIt(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	userID, orgID := newTestUserHome(t, accounts)
	suffix := uuid.New().String()[:8]
	key := func(name, mintedBy string) *domain.APIKey {
		k := &domain.APIKey{ID: name + suffix, OrgID: orgID, CreatedBy: userID, MintedBy: mintedBy, Name: name}
		if err := accounts.CreateAPIKey(ctx, k, "hash-"+name+suffix); err != nil {
			t.Fatalf("create key %s: got = %v, want = nil", name, err)
		}
		return k
	}
	root := key("root", "")
	child := key("child", root.ID)
	grandchild := key("grandchild", child.ID)
	sibling := key("sibling", root.ID)
	unrelated := key("unrelated", "")

	orphan := &domain.APIKey{ID: "orphan" + suffix, OrgID: orgID, CreatedBy: userID, MintedBy: "missing" + suffix, Name: "orphan"}
	if err := accounts.CreateAPIKey(ctx, orphan, "hash-orphan"+suffix); err == nil {
		t.Fatal("a key minted from a key that does not exist: got = nil, want an error")
	}
	if got, err := accounts.GetAPIKey(ctx, grandchild.ID); err != nil || got == nil || got.MintedBy != child.ID || got.CreatedBy != userID {
		t.Fatalf("read a minted key: got = %+v, %v, want it minted by %s", got, err, child.ID)
	}

	if err := accounts.RevokeAPIKey(ctx, child.ID); err != nil {
		t.Fatalf("revoke: got = %v, want = nil", err)
	}
	revoked := func(id string) bool {
		got, err := accounts.GetAPIKey(ctx, id)
		if err != nil || got == nil {
			t.Fatalf("read key %s: got = %+v, %v", id, got, err)
		}
		return got.RevokedAt != nil
	}
	for id, want := range map[string]bool{root.ID: false, child.ID: true, grandchild.ID: true, sibling.ID: false, unrelated.ID: false} {
		if got := revoked(id); got != want {
			t.Errorf("key %s revoked: got = %v, want = %v", id, got, want)
		}
	}

	before, _ := accounts.GetAPIKey(ctx, grandchild.ID)
	if err := accounts.RevokeAPIKey(ctx, root.ID); err != nil {
		t.Fatalf("revoke the root: got = %v, want = nil", err)
	}
	after, _ := accounts.GetAPIKey(ctx, grandchild.ID)
	if !after.RevokedAt.Equal(*before.RevokedAt) {
		t.Fatalf("revoked_at of a key already revoked: got = %v, want it unchanged from %v", after.RevokedAt, before.RevokedAt)
	}
	if !revoked(sibling.ID) || revoked(unrelated.ID) {
		t.Fatal("revoking the root must end every key of its lineage and no other")
	}
}

func TestStaffIsAFlagOnAUserThatExists(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	userID, _ := newTestUserHome(t, accounts)

	if err := accounts.SetUserStaff(ctx, "user-missing-"+uuid.New().String(), true); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("make a missing user staff: got = %v, want = %v", err, ErrUserNotFound)
	}
	for _, want := range []bool{true, false} {
		if err := accounts.SetUserStaff(ctx, userID, want); err != nil {
			t.Fatalf("set staff = %v: got = %v, want = nil", want, err)
		}
		user, orgIDs, err := accounts.EnsureUserHome(ctx, userID, "ignored")
		if err != nil || user.Staff != want || len(orgIDs) != 1 {
			t.Fatalf("user after staff = %v: got = %+v in %v, %v, want the flag set and one org as before", want, user, orgIDs, err)
		}
	}
}

func TestOrgsAreReadByIDAndAnOrgNeedsAnOwnerThatExists(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	userID, homeOrg := newTestUserHome(t, accounts)
	_, otherOrg := newTestUserHome(t, accounts)

	if _, err := accounts.CreateOrg(ctx, "ownerless", "user-missing-"+uuid.New().String()); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("create an org for a missing user: got = %v, want = %v", err, ErrUserNotFound)
	}
	second, err := accounts.CreateOrg(ctx, "home of "+userID, userID)
	if err != nil || second.Role != domain.OrgRoleOwner {
		t.Fatalf("a second org under the name of the first: got = %+v, %v, want it created and owned", second, err)
	}

	got, err := accounts.GetOrg(ctx, second.ID)
	if err != nil || got == nil || got.ID != second.ID || got.Name != "home of "+userID {
		t.Fatalf("get org: got = %+v, %v, want %s", got, err, second.ID)
	}
	if missing, err := accounts.GetOrg(ctx, "org_missing"); err != nil || missing != nil {
		t.Fatalf("get a missing org: got = %+v, %v, want nil, nil", missing, err)
	}
	listed, err := accounts.ListOrgs(ctx, []string{second.ID, homeOrg, "org_missing"})
	if err != nil || len(listed) != 2 || listed[0].ID != homeOrg || listed[1].ID != second.ID || listed[0].Role != "" {
		t.Fatalf("list orgs by id: got = %+v, %v, want the home org then the second, with no role", listed, err)
	}
	all, err := accounts.ListAllOrgs(ctx)
	if err != nil {
		t.Fatalf("list every org: got = %v, want = nil", err)
	}
	seen := map[string]bool{}
	for _, org := range all {
		seen[org.ID] = true
	}
	for _, id := range []string{homeOrg, otherOrg, second.ID, "default"} {
		if !seen[id] {
			t.Errorf("every org: %s is missing", id)
		}
	}
}
