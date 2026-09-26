package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func ownedOrgs(t *testing.T, accounts *AccountsStore, userID string) int {
	t.Helper()
	orgs, err := accounts.ListUserOrgs(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	owned := 0
	for _, org := range orgs {
		if org.Role == domain.OrgRoleOwner {
			owned++
		}
	}
	return owned
}

// The home org of a first sign-in is the first of the orgs a person may own.
func TestAPersonOwnsAtMostTheOrgsTheyMay(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	userID, _ := newTestUserHome(t, accounts)

	for i := 2; i <= 3; i++ {
		if _, err := accounts.CreateOrg(ctx, fmt.Sprintf("org %d", i), userID, 3); err != nil {
			t.Fatalf("org %d of 3: got = %v, want = nil", i, err)
		}
	}
	if _, err := accounts.CreateOrg(ctx, "one too many", userID, 3); !errors.Is(err, ErrMaxOrgsReached) {
		t.Fatalf("a fourth org of 3: got = %v, want = %v", err, ErrMaxOrgsReached)
	}
	if got := ownedOrgs(t, accounts, userID); got != 3 {
		t.Fatalf("orgs owned after the refusal: got = %d, want = 3: a refused create leaves nothing", got)
	}
	var orphans int
	if err := pool.pool.QueryRow(ctx, `SELECT count(*) FROM orgs WHERE name = 'one too many'`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("rows of the refused org: got = %d, %v, want none", orphans, err)
	}
	// An org the person is only a member of takes none of their places, and no limit is none.
	other, otherOrg := newTestUserHome(t, accounts)
	addTestMember(t, accounts, otherOrg, other, userID, domain.OrgRoleMember)
	if _, err := accounts.CreateOrg(ctx, "still refused", userID, 3); !errors.Is(err, ErrMaxOrgsReached) {
		t.Fatalf("a fourth org after joining another as a member: got = %v, want = %v", err, ErrMaxOrgsReached)
	}
	if _, err := accounts.CreateOrg(ctx, "by staff", userID, 0); err != nil {
		t.Fatalf("a create held to no limit: got = %v, want = nil", err)
	}
	if _, err := accounts.CreateOrg(ctx, "for nobody", "user-nobody", 3); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("an org for a user that does not exist: got = %v, want = %v", err, ErrUserNotFound)
	}
}

func TestCreatesAtOnceCannotBothTakeTheLastPlace(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	const callers = 6
	race := func(create func(i int) error, reached error) (won int) {
		t.Helper()
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = create(i)
			}()
		}
		wg.Wait()
		for i, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, reached):
				t.Fatalf("caller %d: got = %v, want nil or %v", i, err, reached)
			}
		}
		return won
	}
	for round := range 5 {
		userID, orgID := newTestUserHome(t, accounts)
		// One place is left of 2 orgs, and two of 3 projects.
		won := race(func(i int) error {
			_, err := accounts.CreateOrg(ctx, fmt.Sprintf("racing %d", i), userID, 2)
			return err
		}, ErrMaxOrgsReached)
		if owned := ownedOrgs(t, accounts, userID); won != 1 || owned != 2 {
			t.Fatalf("round %d: %d org creates won and the person owns %d, want 1 and 2", round, won, owned)
		}
		won = race(func(i int) error {
			_, err := accounts.CreateProject(ctx, orgID, fmt.Sprintf("racing %d", i), 3)
			return err
		}, ErrMaxProjectsReached)
		projects, err := accounts.ListProjects(ctx, []string{orgID})
		if err != nil || won != 2 || len(projects) != 3 {
			t.Fatalf("round %d: %d project creates won and the org holds %d, %v, want 2 and 3", round, won, len(projects), err)
		}
	}
}

func TestAnOrgHoldsAtMostTheProjectsItMay(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	_, orgID := newTestUserHome(t, accounts)

	if _, err := accounts.CreateProject(ctx, orgID, "second", 2); err != nil {
		t.Fatalf("a second project of 2, the default being the first: got = %v, want = nil", err)
	}
	if _, err := accounts.CreateProject(ctx, orgID, "third", 2); !errors.Is(err, ErrMaxProjectsReached) {
		t.Fatalf("a third project of 2: got = %v, want = %v", err, ErrMaxProjectsReached)
	}
	if _, err := accounts.CreateProject(ctx, orgID, "second", 2); !errors.Is(err, ErrProjectNameTaken) {
		t.Fatalf("a taken name in a full org: got = %v, want = %v", err, ErrProjectNameTaken)
	}
	if projects, err := accounts.ListProjects(ctx, []string{orgID}); err != nil || len(projects) != 2 {
		t.Fatalf("projects after the refusals: got = %d, %v, want = 2", len(projects), err)
	}
	if _, err := accounts.CreateProject(ctx, orgID, "by staff", 0); err != nil {
		t.Fatalf("a create held to no limit: got = %v, want = nil", err)
	}
	if _, err := accounts.CreateProject(ctx, "org-nowhere", "x", 2); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("a project in an org that does not exist: got = %v, want = %v", err, ErrOrgNotFound)
	}
}
