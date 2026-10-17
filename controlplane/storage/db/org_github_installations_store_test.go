package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func TestAnOrgConnectsSeveralInstallationsAndFindsOneByItsAccount(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	_, orgA := newTestUserHome(t, accounts)
	_, orgB := newTestUserHome(t, accounts)

	if listed, err := accounts.ListGitHubInstallations(ctx, orgA); err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("installations of a new org: got = %+v, %v, want an empty list", listed, err)
	}
	if err := accounts.DisconnectGitHubInstallation(ctx, orgA, 7); !errors.Is(err, ErrGitHubInstallationNotFound) {
		t.Fatalf("disconnect with none connected: got = %v, want = %v", err, ErrGitHubInstallationNotFound)
	}

	first := &domain.OrgGitHubInstallation{OrgID: orgA, InstallationID: 7, AccountLogin: "Acme", AccountType: "Organization", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}
	if err := accounts.ConnectGitHubInstallation(ctx, first); err != nil || first.ConnectedAt.IsZero() {
		t.Fatalf("connect: got = %v at %v, want = nil and the moment it happened", err, first.ConnectedAt)
	}
	for _, more := range []*domain.OrgGitHubInstallation{
		{OrgID: orgA, InstallationID: 8, AccountLogin: "rival", AccountType: "User", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}},
		// The same installation serves a second org, each with a row of its own.
		{OrgID: orgB, InstallationID: 7, AccountLogin: "Acme", AccountType: "Organization", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-2"}},
	} {
		if err := accounts.ConnectGitHubInstallation(ctx, more); err != nil {
			t.Fatalf("connect %+v: %v", more, err)
		}
	}

	// Connected again, it keeps who connected it and when, and takes the account's name of today.
	again := &domain.OrgGitHubInstallation{OrgID: orgA, InstallationID: 7, AccountLogin: "Acme-Renamed", AccountType: "Organization", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-9"}}
	if err := accounts.ConnectGitHubInstallation(ctx, again); err != nil {
		t.Fatalf("connect again: %v", err)
	}
	if again.ConnectedActor.ID != "user-1" || !again.ConnectedAt.Equal(first.ConnectedAt) {
		t.Fatalf("connected again = %+v, want who and when of the first connect (%s at %s)", again, first.ConnectedActor.ID, first.ConnectedAt.Format(time.RFC3339Nano))
	}

	listed, err := accounts.ListGitHubInstallations(ctx, orgA)
	if err != nil || len(listed) != 2 || listed[0].InstallationID != 7 || listed[0].AccountLogin != "Acme-Renamed" || listed[0].AccountType != "Organization" || listed[1].InstallationID != 8 || listed[1].AccountType != "User" {
		t.Fatalf("list: got = %+v, %v, want the org's two, oldest first, the first under its new name, each with the type of its account", listed, err)
	}

	// A row from before the type was kept reads with none, and connecting it again records it.
	if _, err := pool.pool.Exec(ctx, `UPDATE org_github_installations SET account_type = '' WHERE org_id = $1 AND installation_id = 7`, orgA); err != nil {
		t.Fatal(err)
	}
	if found, err := accounts.GitHubInstallationOfAccount(ctx, orgA, "acme-renamed"); err != nil || found == nil || found.AccountType != "" {
		t.Fatalf("a row from before the type was kept: got = %+v, %v, want it read with no type", found, err)
	}
	if err := accounts.ConnectGitHubInstallation(ctx, again); err != nil || again.AccountType != "Organization" {
		t.Fatalf("connected again = %+v, %v, want the type recorded", again, err)
	}
	if err := accounts.ConnectGitHubInstallation(ctx, &domain.OrgGitHubInstallation{OrgID: orgA, InstallationID: 9, AccountLogin: "untyped", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}); err == nil {
		t.Fatal("an installation was connected without the type of its account")
	}

	for login, want := range map[string]int64{"acme-renamed": 7, "ACME-RENAMED": 7, "Rival": 8} {
		found, err := accounts.GitHubInstallationOfAccount(ctx, orgA, login)
		if err != nil || found == nil || found.InstallationID != want || found.OrgID != orgA {
			t.Errorf("installation of account %q: got = %+v, %v, want installation %d", login, found, err, want)
		}
	}
	for name, lookup := range map[string][2]string{
		"an account the org did not connect": {orgA, "nobody"},
		"the name the account had before":    {orgA, "Acme"},
		"an account only another org holds":  {orgB, "rival"},
		"a pattern for a name":               {orgA, "%"},
	} {
		if found, err := accounts.GitHubInstallationOfAccount(ctx, lookup[0], lookup[1]); err != nil || found != nil {
			t.Errorf("%s: got = %+v, %v, want none", name, found, err)
		}
	}

	if err := accounts.DisconnectGitHubInstallation(ctx, orgA, 7); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if err := accounts.DisconnectGitHubInstallation(ctx, orgA, 7); !errors.Is(err, ErrGitHubInstallationNotFound) {
		t.Fatalf("disconnect twice: got = %v, want = %v", err, ErrGitHubInstallationNotFound)
	}
	if found, err := accounts.GitHubInstallationOfAccount(ctx, orgB, "acme"); err != nil || found == nil || found.InstallationID != 7 {
		t.Fatalf("after one org disconnected it, the other's: got = %+v, %v, want it still connected", found, err)
	}

	if err := accounts.ConnectGitHubInstallation(ctx, &domain.OrgGitHubInstallation{OrgID: "org-nowhere", InstallationID: 7, AccountLogin: "Acme", AccountType: "Organization", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("connect to an org that does not exist: got = %v, want = %v", err, ErrOrgNotFound)
	}
}

// One installation per org is where a new repository goes when a request names no account: the
// org's first, until another is made the default. A new installation on an account the org
// connected replaces the one GitHub removed there, default and all.
func TestOneInstallationIsTheOrgsDefaultAndANewOneOnItsAccountReplacesIt(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	_, org := newTestUserHome(t, accounts)
	by := domain.Actor{Kind: domain.ActorUser, ID: "user-1"}
	for _, one := range []*domain.OrgGitHubInstallation{
		{OrgID: org, InstallationID: 7, AccountLogin: "Acme", AccountType: "Organization", ConnectedActor: by},
		{OrgID: org, InstallationID: 8, AccountLogin: "carol", AccountType: "User", ConnectedActor: by},
	} {
		first := one.InstallationID == 7
		if err := accounts.ConnectGitHubInstallation(ctx, one); err != nil || one.Default != first {
			t.Fatalf("connect %d: %v, default %v, want it connected and the default only as the org's first", one.InstallationID, err, one.Default)
		}
	}
	defaults := func() []int64 {
		t.Helper()
		listed, err := accounts.ListGitHubInstallations(ctx, org)
		if err != nil {
			t.Fatal(err)
		}
		out := []int64{}
		for _, one := range listed {
			if one.Default {
				out = append(out, one.InstallationID)
			}
		}
		return out
	}
	for _, id := range []int64{7, 8, 8} {
		if err := accounts.MakeGitHubInstallationDefault(ctx, org, id); err != nil || fmt.Sprint(defaults()) != fmt.Sprint([]int64{id}) {
			t.Fatalf("make %d the default: %v, defaults %v, want it alone", id, err, defaults())
		}
	}
	if err := accounts.MakeGitHubInstallationDefault(ctx, org, 99); !errors.Is(err, ErrGitHubInstallationNotFound) || fmt.Sprint(defaults()) != "[8]" {
		t.Fatalf("make one the org did not connect the default: %v, defaults %v, want %v and 8 kept", err, defaults(), ErrGitHubInstallationNotFound)
	}
	reinstalled := &domain.OrgGitHubInstallation{OrgID: org, InstallationID: 18, AccountLogin: "Carol", AccountType: "User", ConnectedActor: by}
	if err := accounts.ConnectGitHubInstallation(ctx, reinstalled); err != nil || !reinstalled.Default {
		t.Fatalf("connect a new installation on carol: %+v, %v, want it the default in place of the one removed", reinstalled, err)
	}
	listed, err := accounts.ListGitHubInstallations(ctx, org)
	if err != nil || len(listed) != 2 || listed[0].InstallationID != 7 || listed[1].InstallationID != 18 {
		t.Fatalf("listed = %+v, %v, want Acme's and the new one on carol, the removed one gone", listed, err)
	}
	if err := accounts.DisconnectGitHubInstallation(ctx, org, 18); err != nil || len(defaults()) != 0 {
		t.Fatalf("disconnect the default: %v, defaults %v, want none left", err, defaults())
	}
}

// An install state works once, for the actor it was issued to, until it runs out, and names the
// org and the page the install was started from.
func TestAnInstallStateIsTakenOnceByThePersonItWasIssuedToWhileItHolds(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	_, org := newTestUserHome(t, accounts)
	alice := domain.Actor{Kind: domain.ActorUser, ID: "user-alice"}
	bob := domain.Actor{Kind: domain.ActorUser, ID: "user-bob"}

	before := time.Now().Add(-time.Second)
	state, expires, err := accounts.IssueGitHubInstallState(ctx, org, alice, "/settings", 15*time.Minute)
	if err != nil || len(state) < 40 || expires.Before(before.Add(14*time.Minute)) {
		t.Fatalf("issue: got = %q until %v, %v, want a long random state good for 15 minutes", state, expires, err)
	}
	var stored []byte
	if err := pool.pool.QueryRow(ctx, `SELECT state_hash FROM github_install_states`).Scan(&stored); err != nil || bytes.Contains(stored, []byte(state)) {
		t.Fatalf("the state is stored in the clear: %v", err)
	}
	if _, err := accounts.TakeGitHubInstallState(ctx, state, bob); !errors.Is(err, ErrGitHubInstallStateInvalid) {
		t.Fatalf("another person takes it: got = %v, want = %v", err, ErrGitHubInstallStateInvalid)
	}
	if _, err := accounts.TakeGitHubInstallState(ctx, "made-up", alice); !errors.Is(err, ErrGitHubInstallStateInvalid) {
		t.Fatalf("a made-up state: got = %v, want = %v", err, ErrGitHubInstallStateInvalid)
	}
	taken, err := accounts.TakeGitHubInstallState(ctx, state, alice)
	if err != nil || taken.OrgID != org || taken.ReturnTo != "/settings" || taken.Actor != alice || taken.IssuedAt.Before(before) {
		t.Fatalf("take: got = %+v, %v, want the org, the page and when it was issued", taken, err)
	}
	if _, err := accounts.TakeGitHubInstallState(ctx, state, alice); !errors.Is(err, ErrGitHubInstallStateInvalid) {
		t.Fatalf("taken twice: got = %v, want = %v", err, ErrGitHubInstallStateInvalid)
	}

	expired, _, err := accounts.IssueGitHubInstallState(ctx, org, alice, "/settings", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.pool.Exec(ctx, `UPDATE github_install_states SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.TakeGitHubInstallState(ctx, expired, alice); !errors.Is(err, ErrGitHubInstallStateInvalid) {
		t.Fatalf("an expired state: got = %v, want = %v", err, ErrGitHubInstallStateInvalid)
	}
	if _, _, err := accounts.IssueGitHubInstallState(ctx, org, alice, "/settings", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.pool.QueryRow(ctx, `SELECT count(*) FROM github_install_states WHERE expires_at <= now()`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("expired states left after an issue: %d, %v, want none", left, err)
	}
	if _, _, err := accounts.IssueGitHubInstallState(ctx, "org-nowhere", alice, "/settings", 15*time.Minute); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("issue for an org that does not exist: got = %v, want = %v", err, ErrOrgNotFound)
	}
	var personal bool
	if err := pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'org_github_installations' AND column_name IN ('user_refresh_token', 'user_authorization'))`).Scan(&personal); err != nil || personal {
		t.Fatalf("a column that kept a person's GitHub authorisation exists = %v, %v, want both dropped", personal, err)
	}
}

func TestARepositorysAddressIsStoredAtCreation(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	now := time.Now().UTC()
	create := func(repository, address string) string {
		t.Helper()
		pe := &domain.PlatformExperiment{Actor: testActor, DefaultQuotaTier: domain.QuotaTierGuaranteed,
			ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "with code", BudgetAcceleratorHours: 10,
			MaxAgents: 10, StartsAt: now, EndsAt: now.Add(time.Hour), Status: domain.PlatformExpOpen,
			Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
			Stages: []domain.Stage{{LengthPct: 100}}, CreatedAt: now, UpdatedAt: now,
			Repository: repository, RepositoryURL: address,
		}
		if err := pes.CreatePlatformExperiment(ctx, pe, nil, nil); err != nil {
			t.Fatalf("create: %v", err)
		}
		return pe.ID
	}
	address := func(id string) string {
		t.Helper()
		got, err := pes.GetPlatformExperiment(ctx, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return got.RepositoryURL
	}

	elsewhere := create("group/model", "https://git.example/experiments/group/model")
	bare := create("", "")
	for name, tc := range map[string][2]string{
		"an address stored at creation":            {elsewhere, "https://git.example/experiments/group/model"},
		"a platform experiment with no repository": {bare, ""},
	} {
		if got := address(tc[0]); got != tc[1] {
			t.Errorf("%s: repository_url = %q, want %q", name, got, tc[1])
		}
	}
	if project, repository, closed, draft, err := pes.PlatformExperimentRepository(ctx, elsewhere); err != nil || project != "default" || repository != "group/model" || closed || draft {
		t.Fatalf("PlatformExperimentRepository = %q, %q, %v, %v, %v, want the project and the name of one still open and past its draft", project, repository, closed, draft, err)
	}
	if err := pes.ClosePlatformExperiment(ctx, elsewhere, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, closed, _, err := pes.PlatformExperimentRepository(ctx, elsewhere); err != nil || !closed {
		t.Fatalf("PlatformExperimentRepository of a closed one: closed %v, %v, want it closed", closed, err)
	}
}
