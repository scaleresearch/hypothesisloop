package db

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// newTestInvite stores an invite made by createdBy and returns it with the hash it is accepted under.
func newTestInvite(t *testing.T, accounts *AccountsStore, orgID, createdBy string, role domain.OrgRole, lifetime time.Duration) (*domain.OrgInvite, string) {
	t.Helper()
	suffix := uuid.New().String()
	invite := &domain.OrgInvite{ID: "inv-" + suffix, OrgID: orgID, Role: role, CreatedBy: createdBy, ExpiresAt: time.Now().Add(lifetime)}
	if err := accounts.CreateOrgInvite(context.Background(), invite, "hash-"+suffix); err != nil {
		t.Fatalf("create invite: got = %v, want = nil", err)
	}
	return invite, "hash-" + suffix
}

// addTestMember brings userID into the org the way a person joins one: by an invite of an owner.
func addTestMember(t *testing.T, accounts *AccountsStore, orgID, ownerID, userID string, role domain.OrgRole) {
	t.Helper()
	_, hash := newTestInvite(t, accounts, orgID, ownerID, role, domain.InviteLifetime)
	if _, err := accounts.AcceptOrgInvite(context.Background(), hash, userID); err != nil {
		t.Fatalf("accept invite as %s: got = %v, want = nil", userID, err)
	}
}

func pendingInviteIDs(t *testing.T, accounts *AccountsStore, orgID string) []string {
	t.Helper()
	pending, total, err := accounts.ListPendingOrgInvites(context.Background(), orgID, 200, 0)
	if err == nil && total != len(pending) {
		t.Fatalf("pending invites: total = %d, listed = %d, want them equal on one page", total, len(pending))
	}
	if err != nil {
		t.Fatalf("list pending invites: got = %v, want = nil", err)
	}
	ids := make([]string, len(pending))
	for i, invite := range pending {
		ids[i] = invite.ID
	}
	return ids
}

func memberRoles(t *testing.T, accounts *AccountsStore, orgID string) map[string]domain.OrgRole {
	t.Helper()
	members, total, err := accounts.ListOrgMembers(context.Background(), orgID, 200, 0)
	if err == nil && total != len(members) {
		t.Fatalf("members: total = %d, listed = %d, want them equal on one page", total, len(members))
	}
	if err != nil {
		t.Fatalf("list members: got = %v, want = nil", err)
	}
	roles := map[string]domain.OrgRole{}
	for _, m := range members {
		roles[m.UserID] = m.Role
	}
	return roles
}

func TestAnInviteMakesOneMemberWithItsRole(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	owner, orgID := newTestUserHome(t, accounts)
	guest, guestOrg := newTestUserHome(t, accounts)
	late, _ := newTestUserHome(t, accounts)

	invite, hash := newTestInvite(t, accounts, orgID, owner, domain.OrgRoleMember, domain.InviteLifetime)
	if got := pendingInviteIDs(t, accounts, orgID); !slices.Equal(got, []string{invite.ID}) {
		t.Fatalf("pending invites: got = %v, want = [%s]", got, invite.ID)
	}
	member, err := accounts.AcceptOrgInvite(ctx, hash, guest)
	if err != nil || member.OrgID != orgID || member.UserID != guest || member.Role != domain.OrgRoleMember || member.JoinedAt.IsZero() {
		t.Fatalf("accept: got = %+v, %v, want %s a member of %s", member, err, guest, orgID)
	}
	if got := memberRoles(t, accounts, orgID); len(got) != 2 || got[owner] != domain.OrgRoleOwner || got[guest] != domain.OrgRoleMember {
		t.Fatalf("members: got = %v, want the owner and the new member", got)
	}
	if _, orgIDs, err := accounts.EnsureUserHome(ctx, guest, "ignored"); err != nil || !slices.Equal(orgIDs, []string{guestOrg, orgID}) {
		t.Fatalf("orgs of the one who joined: got = %v, %v, want home first then the joined org", orgIDs, err)
	}
	if got := pendingInviteIDs(t, accounts, orgID); len(got) != 0 {
		t.Fatalf("pending invites after the accept: got = %v, want none", got)
	}
	if _, err := accounts.AcceptOrgInvite(ctx, hash, late); !errors.Is(err, ErrOrgInviteUsed) {
		t.Fatalf("a second accept: got = %v, want = %v", err, ErrOrgInviteUsed)
	}
	if _, found, _ := accounts.OrgRole(ctx, orgID, late); found {
		t.Fatal("the second accept made a member")
	}
	if page, total, err := accounts.ListOrgMembers(ctx, orgID, 1, 1); err != nil || total != 2 || len(page) != 1 || page[0].UserID != guest {
		t.Fatalf("the second page of one: got = %+v of %d, %v, want the one who joined of 2", page, total, err)
	}
	if err := accounts.RevokeOrgInvite(ctx, invite.ID); !errors.Is(err, ErrOrgInviteUsed) {
		t.Fatalf("revoking an accepted invite: got = %v, want = %v", err, ErrOrgInviteUsed)
	}
}

func TestOfTwoAcceptsOfOneInviteAtOnceExactlyOneWins(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	owner, orgID := newTestUserHome(t, accounts)
	const callers = 8
	users := make([]string, callers)
	for i := range users {
		users[i], _ = newTestUserHome(t, accounts)
	}
	_, hash := newTestInvite(t, accounts, orgID, owner, domain.OrgRoleMember, domain.InviteLifetime)

	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = accounts.AcceptOrgInvite(context.Background(), hash, users[i])
		}()
	}
	wg.Wait()
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrOrgInviteUsed):
			t.Errorf("caller %d: got = %v, want nil or %v", i, err, ErrOrgInviteUsed)
		}
	}
	if got := memberRoles(t, accounts, orgID); won != 1 || len(got) != 2 {
		t.Fatalf("%d accepts won and the org has %d members, want one winner and the owner beside them", won, len(got))
	}
}

func TestAnInviteAcceptedByAMemberIsRefusedAndStaysUnused(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	owner, orgID := newTestUserHome(t, accounts)
	guest, _ := newTestUserHome(t, accounts)

	invite, hash := newTestInvite(t, accounts, orgID, owner, domain.OrgRoleMember, domain.InviteLifetime)
	if _, err := accounts.AcceptOrgInvite(ctx, hash, owner); !errors.Is(err, ErrAlreadyOrgMember) {
		t.Fatalf("an owner accepting an invite to their own org: got = %v, want = %v", err, ErrAlreadyOrgMember)
	}
	if role, _, _ := accounts.OrgRole(ctx, orgID, owner); role != domain.OrgRoleOwner {
		t.Fatalf("role of the owner after the refused accept: got = %q, want owner", role)
	}
	if got := pendingInviteIDs(t, accounts, orgID); !slices.Equal(got, []string{invite.ID}) {
		t.Fatalf("pending invites after the refused accept: got = %v, want it still pending", got)
	}
	if _, err := accounts.AcceptOrgInvite(ctx, hash, guest); err != nil {
		t.Fatalf("the invite, accepted by someone new after a member tried it: got = %v, want = nil", err)
	}
}

func TestAnInviteThatExpiredWasRevokedOrNeverExistedAdmitsNobody(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	owner, orgID := newTestUserHome(t, accounts)
	guest, _ := newTestUserHome(t, accounts)

	expired, expiredHash := newTestInvite(t, accounts, orgID, owner, domain.OrgRoleMember, -time.Second)
	revoked, revokedHash := newTestInvite(t, accounts, orgID, owner, domain.OrgRoleOwner, domain.InviteLifetime)
	live, _ := newTestInvite(t, accounts, orgID, owner, domain.OrgRoleMember, domain.InviteLifetime)

	if err := accounts.RevokeOrgInvite(ctx, "inv-missing"); !errors.Is(err, ErrOrgInviteNotFound) {
		t.Fatalf("revoking an invite that does not exist: got = %v, want = %v", err, ErrOrgInviteNotFound)
	}
	if missing, err := accounts.GetOrgInvite(ctx, "inv-missing"); err != nil || missing != nil {
		t.Fatalf("reading an invite that does not exist: got = %+v, %v, want nil, nil", missing, err)
	}
	if err := accounts.RevokeOrgInvite(ctx, revoked.ID); err != nil {
		t.Fatalf("revoke: got = %v, want = nil", err)
	}
	first, err := accounts.GetOrgInvite(ctx, revoked.ID)
	if err != nil || first == nil || first.RevokedAt == nil || first.OrgID != orgID || first.Role != domain.OrgRoleOwner {
		t.Fatalf("the revoked invite: got = %+v, %v, want it revoked", first, err)
	}
	if err := accounts.RevokeOrgInvite(ctx, revoked.ID); err != nil {
		t.Fatalf("revoking a revoked invite: got = %v, want = nil", err)
	}
	if again, _ := accounts.GetOrgInvite(ctx, revoked.ID); !again.RevokedAt.Equal(*first.RevokedAt) {
		t.Fatalf("revoked_at of an invite already revoked: got = %v, want it unchanged from %v", again.RevokedAt, first.RevokedAt)
	}
	if got := pendingInviteIDs(t, accounts, orgID); !slices.Equal(got, []string{live.ID}) {
		t.Fatalf("pending invites: got = %v, want only %s: %s expired and %s was revoked", got, live.ID, expired.ID, revoked.ID)
	}
	for hash, want := range map[string]error{expiredHash: ErrOrgInviteExpired, revokedHash: ErrOrgInviteRevoked, "hash-of-no-invite": ErrOrgInviteNotFound} {
		if _, err := accounts.AcceptOrgInvite(ctx, hash, guest); !errors.Is(err, want) {
			t.Errorf("accept under %s: got = %v, want = %v", hash, err, want)
		}
	}
	if _, found, _ := accounts.OrgRole(ctx, orgID, guest); found {
		t.Fatal("a refused accept made a member")
	}
}

func TestAnOrgKeepsItsLastOwner(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	owner, orgID := newTestUserHome(t, accounts)
	guest, _ := newTestUserHome(t, accounts)
	addTestMember(t, accounts, orgID, owner, guest, domain.OrgRoleMember)

	if _, err := accounts.SetOrgMemberRole(ctx, orgID, owner, domain.OrgRoleMember); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("demoting the only owner: got = %v, want = %v", err, ErrLastOwner)
	}
	if err := accounts.RemoveOrgMember(ctx, orgID, owner); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("removing the only owner: got = %v, want = %v", err, ErrLastOwner)
	}
	if got := memberRoles(t, accounts, orgID); got[owner] != domain.OrgRoleOwner || got[guest] != domain.OrgRoleMember {
		t.Fatalf("members after the refusals: got = %v, want them unchanged", got)
	}
	for name, err := range map[string]error{
		"a role for someone outside the org": func() error {
			_, err := accounts.SetOrgMemberRole(ctx, orgID, "user-nobody", domain.OrgRoleOwner)
			return err
		}(),
		"removing someone outside the org": accounts.RemoveOrgMember(ctx, orgID, "user-nobody"),
		"a role in an org that is not there": func() error {
			_, err := accounts.SetOrgMemberRole(ctx, "org-nowhere", owner, domain.OrgRoleOwner)
			return err
		}(),
		"removing from an org not there": accounts.RemoveOrgMember(ctx, "org-nowhere", owner),
	} {
		if !errors.Is(err, ErrOrgMemberNotFound) {
			t.Errorf("%s: got = %v, want = %v", name, err, ErrOrgMemberNotFound)
		}
	}

	promoted, err := accounts.SetOrgMemberRole(ctx, orgID, guest, domain.OrgRoleOwner)
	if err != nil || promoted.Role != domain.OrgRoleOwner {
		t.Fatalf("promote: got = %+v, %v, want an owner", promoted, err)
	}
	if err := accounts.RemoveOrgMember(ctx, orgID, owner); err != nil {
		t.Fatalf("an owner leaving an org that keeps another: got = %v, want = nil", err)
	}
	if got := memberRoles(t, accounts, orgID); len(got) != 1 || got[guest] != domain.OrgRoleOwner {
		t.Fatalf("members: got = %v, want only the remaining owner", got)
	}
}

// Each of two owners stops being one at the same moment. A check made before the write would let
// both through, each seeing the other as the owner that stays.
func TestTwoOwnersSteppingDownAtOnceLeaveOne(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	stepDown := map[string]func(orgID, userID string) error{
		"demoted": func(orgID, userID string) error {
			_, err := accounts.SetOrgMemberRole(ctx, orgID, userID, domain.OrgRoleMember)
			return err
		},
		"removed": func(orgID, userID string) error { return accounts.RemoveOrgMember(ctx, orgID, userID) },
	}
	for name, change := range stepDown {
		for round := range 10 {
			first, orgID := newTestUserHome(t, accounts)
			second, _ := newTestUserHome(t, accounts)
			addTestMember(t, accounts, orgID, first, second, domain.OrgRoleOwner)

			errs := make([]error, 2)
			var wg sync.WaitGroup
			for i, userID := range []string{first, second} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = change(orgID, userID)
				}()
			}
			wg.Wait()
			refused := 0
			for _, err := range errs {
				switch {
				case errors.Is(err, ErrLastOwner):
					refused++
				case err != nil:
					t.Fatalf("%s, round %d: got = %v, want nil or %v", name, round, err, ErrLastOwner)
				}
			}
			owners := 0
			for _, role := range memberRoles(t, accounts, orgID) {
				if role == domain.OrgRoleOwner {
					owners++
				}
			}
			if refused != 1 || owners != 1 {
				t.Fatalf("%s, round %d: %d of two were refused and %d owners remain, want one refused and one owner", name, round, refused, owners)
			}
		}
	}
}

// What a person holds in an org goes with them: the keys they made stop authenticating and the
// invites they made are revoked. What they did stays: their account, the invite they came in by
// and what they wrote.
func TestWhatAPersonHeldInAnOrgEndsWhenTheyLeaveIt(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	owner, orgID := newTestUserHome(t, accounts)
	second, secondHome := newTestUserHome(t, accounts)
	member, _ := newTestUserHome(t, accounts)
	project := defaultProjectOf(t, accounts, orgID)
	suffix := uuid.New().String()[:8]
	key := func(name, createdBy, projectID, mintedBy string) error {
		k := &domain.APIKey{ID: name + suffix, OrgID: orgID, ProjectID: projectID, CreatedBy: createdBy, MintedBy: mintedBy, Name: name}
		return accounts.CreateAPIKey(ctx, k, "hash-"+name+suffix)
	}
	working := func(name string) bool {
		t.Helper()
		got, err := accounts.APIKeyByHash(ctx, "hash-"+name+suffix)
		if err != nil || got == nil {
			t.Fatalf("read key %s: got = %+v, %v, want it found", name, got, err)
		}
		return works(got)
	}

	if err := key("early", member, project.ID, ""); !errors.Is(err, ErrAPIKeyCreatorLacksOrg) {
		t.Fatalf("a key in the name of someone outside the org: got = %v, want = %v", err, ErrAPIKeyCreatorLacksOrg)
	}
	joinedBy, joinedHash := newTestInvite(t, accounts, orgID, owner, domain.OrgRoleOwner, domain.InviteLifetime)
	if _, err := accounts.AcceptOrgInvite(ctx, joinedHash, second); err != nil {
		t.Fatal(err)
	}
	addTestMember(t, accounts, orgID, owner, member, domain.OrgRoleMember)

	if err := key("member-org-wide", member, "", ""); !errors.Is(err, ErrAPIKeyCreatorLacksOrg) {
		t.Fatalf("an org-wide key in the name of a member: got = %v, want = %v", err, ErrAPIKeyCreatorLacksOrg)
	}
	for _, k := range []struct{ name, createdBy, projectID, mintedBy string }{
		{"member-project", member, project.ID, ""},
		{"member-minted", member, project.ID, "member-project" + suffix},
		{"second-org-wide", second, "", ""},
		{"second-project", second, project.ID, ""},
		{"owner-org-wide", owner, "", ""},
	} {
		if err := key(k.name, k.createdBy, k.projectID, k.mintedBy); err != nil {
			t.Fatalf("create key %s: got = %v, want = nil", k.name, err)
		}
		if !working(k.name) {
			t.Fatalf("key %s does not work while its creator holds the org", k.name)
		}
	}
	bySecond, _ := newTestInvite(t, accounts, orgID, second, domain.OrgRoleOwner, domain.InviteLifetime)
	byOwner, _ := newTestInvite(t, accounts, orgID, owner, domain.OrgRoleMember, domain.InviteLifetime)
	createTestPEIn(t, pool, "pe-leaving-"+suffix, project.ID)
	if _, err := pool.pool.Exec(ctx, `
INSERT INTO human_writes (user_id, author, kind, subject, platform_experiment_id) VALUES ($1, 'Member', 'comment', 'c-1', $2)`,
		member, "pe-leaving-"+suffix); err != nil {
		t.Fatal(err)
	}

	if _, err := accounts.SetOrgMemberRole(ctx, orgID, second, domain.OrgRoleMember); err != nil {
		t.Fatal(err)
	}
	if working("second-org-wide") || !working("second-project") {
		t.Fatal("once an owner is a member, their org-wide key must stop and their project key must not: a member may hold the one and not the other")
	}
	if got := pendingInviteIDs(t, accounts, orgID); !slices.Equal(got, []string{byOwner.ID}) {
		t.Fatalf("pending invites after the demotion: got = %v, want only %s: %s was made by the one demoted", got, byOwner.ID, bySecond.ID)
	}

	if err := accounts.RemoveOrgMember(ctx, orgID, member); err != nil {
		t.Fatal(err)
	}
	if err := accounts.RemoveOrgMember(ctx, orgID, second); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"member-project", "member-minted", "second-org-wide", "second-project"} {
		if working(name) {
			t.Errorf("key %s still works after its creator left the org", name)
		}
	}
	if !working("owner-org-wide") {
		t.Error("the key of the owner who stayed stopped working")
	}
	listed, err := accounts.ListAPIKeys(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range listed {
		if want := k.CreatedBy != owner; k.Orphaned != want {
			t.Errorf("key %s listed orphaned = %v, want = %v", k.ID, k.Orphaned, want)
		}
	}
	if _, found, _ := accounts.OrgRole(ctx, orgID, member); found {
		t.Error("the one removed still has a role in the org")
	}
	if _, orgIDs, err := accounts.EnsureUserHome(ctx, second, "ignored"); err != nil || !slices.Equal(orgIDs, []string{secondHome}) {
		t.Errorf("orgs of the one who left: got = %v, %v, want their home alone and no new one made", orgIDs, err)
	}
	var writes int
	var acceptedBy string
	if err := pool.pool.QueryRow(ctx, `SELECT count(*) FROM human_writes WHERE user_id = $1`, member).Scan(&writes); err != nil || writes != 1 {
		t.Errorf("what the one removed wrote: got = %d rows, %v, want it kept", writes, err)
	}
	if err := pool.pool.QueryRow(ctx, `SELECT accepted_by FROM org_invites WHERE id = $1`, joinedBy.ID).Scan(&acceptedBy); err != nil || acceptedBy != second {
		t.Errorf("the invite the one who left came in by: got accepted_by = %q, %v, want it kept", acceptedBy, err)
	}

	// Coming back by a new invite brings the project keys back with the membership they hang on.
	addTestMember(t, accounts, orgID, owner, member, domain.OrgRoleMember)
	if !working("member-project") || !working("member-minted") {
		t.Error("a key follows its creator's standing as it is now: back in the org, their project keys work again")
	}
}
