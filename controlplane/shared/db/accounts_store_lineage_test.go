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

// The order a mint racing a revocation can take: the minter is read while it works, it is revoked,
// and only then is the child written.
func works(k *domain.APIKey) bool { return k.RevokedAt == nil && !k.Orphaned }

func TestAKeyMintedWhileItsParentIsRevokedNeverWorks(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	userID, orgID := newTestUserHome(t, accounts)
	suffix := uuid.New().String()[:8]
	parent := &domain.APIKey{ID: "parent" + suffix, OrgID: orgID, CreatedBy: userID, Name: "parent"}
	if err := accounts.CreateAPIKey(ctx, parent, "hash-parent"+suffix); err != nil {
		t.Fatal(err)
	}

	minter, err := accounts.GetAPIKey(ctx, parent.ID)
	if err != nil || minter == nil || !works(minter) {
		t.Fatalf("the minter as the mint read it: got = %+v, %v, want a working key", minter, err)
	}
	if err := accounts.RevokeAPIKey(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	child := &domain.APIKey{ID: "child" + suffix, OrgID: orgID, CreatedBy: minter.CreatedBy, MintedBy: minter.ID, Name: "child"}
	if err := accounts.CreateAPIKey(ctx, child, "hash-child"+suffix); err != nil {
		t.Fatalf("the late mint: got = %v, want it written, as the race writes it", err)
	}
	grandchild := &domain.APIKey{ID: "grandchild" + suffix, OrgID: orgID, CreatedBy: userID, MintedBy: child.ID, Name: "grandchild"}
	if err := accounts.CreateAPIKey(ctx, grandchild, "hash-grandchild"+suffix); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"child", "grandchild"} {
		got, err := accounts.APIKeyByHash(ctx, "hash-"+name+suffix)
		if err != nil || got == nil || works(got) || got.RevokedAt == nil {
			t.Fatalf("authenticating the %s minted after the revocation: got = %+v, %v, want a revoked key", name, got, err)
		}
	}
	listed, err := accounts.ListAPIKeys(ctx, orgID)
	if err != nil || len(listed) != 3 {
		t.Fatalf("list: got = %d keys, %v, want 3", len(listed), err)
	}
	for _, k := range listed {
		if works(k) {
			t.Errorf("key %s is listed as working", k.ID)
		}
	}
}

func TestAKeyEndsWhenItsCreatorNoLongerAdministersItsOrg(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	ownerID, orgID := newTestUserHome(t, accounts)
	staffID, _ := newTestUserHome(t, accounts)
	suffix := uuid.New().String()[:8]
	working := func(name string) bool {
		t.Helper()
		got, err := accounts.APIKeyByHash(ctx, "hash-"+name+suffix)
		if err != nil || got == nil || got.RevokedAt != nil {
			t.Fatalf("read key %s: got = %+v, %v, want it found and not revoked", name, got, err)
		}
		return works(got)
	}
	key := func(name, createdBy, mintedBy string) {
		t.Helper()
		k := &domain.APIKey{ID: name + suffix, OrgID: orgID, CreatedBy: createdBy, MintedBy: mintedBy, Name: name}
		if err := accounts.CreateAPIKey(ctx, k, "hash-"+name+suffix); err != nil {
			t.Fatal(err)
		}
	}

	early := &domain.APIKey{ID: "early" + suffix, OrgID: orgID, CreatedBy: staffID, Name: "early"}
	if err := accounts.CreateAPIKey(ctx, early, "hash-early"+suffix); !errors.Is(err, ErrAPIKeyCreatorLacksOrg) {
		t.Fatalf("a key in the name of a user who neither owns the org nor is staff: got = %v, want = %v", err, ErrAPIKeyCreatorLacksOrg)
	}
	if got, err := accounts.APIKeyByHash(ctx, "hash-early"+suffix); err != nil || got != nil {
		t.Fatalf("the refused key: got = %+v, %v, want no row", got, err)
	}
	if err := accounts.SetUserStaff(ctx, staffID, true); err != nil {
		t.Fatal(err)
	}
	key("by-staff", staffID, "")
	key("by-owner", ownerID, "")
	key("minted", ownerID, "by-owner"+suffix)
	if !working("by-staff") || !working("by-owner") || !working("minted") {
		t.Fatal("keys of a staff user and of the org's owner: want all three working")
	}

	if err := accounts.SetUserStaff(ctx, staffID, false); err != nil {
		t.Fatal(err)
	}
	if working("by-staff") || !working("by-owner") {
		t.Fatal("once its creator is no longer staff and never owned the org, the key must stop; the owner's must not")
	}
	if err := accounts.SetUserStaff(ctx, staffID, true); err != nil {
		t.Fatal(err)
	}
	if !working("by-staff") {
		t.Fatal("a key follows its creator's standing as it is now")
	}

	addTestMember(t, accounts, orgID, ownerID, staffID, domain.OrgRoleOwner)
	if _, err := accounts.SetOrgMemberRole(ctx, orgID, ownerID, domain.OrgRoleMember); err != nil {
		t.Fatal(err)
	}
	if working("by-owner") || working("minted") {
		t.Fatal("once its creator is demoted to member, the key and the key minted from it must stop")
	}
	listed, err := accounts.ListAPIKeys(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range listed {
		if want := k.CreatedBy == ownerID; k.Orphaned != want {
			t.Errorf("key %s listed orphaned = %v, want = %v", k.ID, k.Orphaned, want)
		}
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
