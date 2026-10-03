package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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

	// A row from before the type was kept holds none until GitHub's answer is recorded, on
	// every org's record of the installation.
	if _, err := pool.pool.Exec(ctx, `UPDATE org_github_installations SET account_type = '' WHERE installation_id = 7`); err != nil {
		t.Fatal(err)
	}
	if found, err := accounts.GitHubInstallationOfAccount(ctx, orgA, "acme-renamed"); err != nil || found == nil || found.AccountType != "" {
		t.Fatalf("a row from before the type was kept: got = %+v, %v, want it read with no type", found, err)
	}
	if err := accounts.SetGitHubInstallationAccountType(ctx, 7, "Organization"); err != nil {
		t.Fatalf("set the account type: %v", err)
	}
	for _, org := range []string{orgA, orgB} {
		if found, err := accounts.GitHubInstallationOfAccount(ctx, org, map[string]string{orgA: "acme-renamed", orgB: "acme"}[org]); err != nil || found == nil || found.AccountType != "Organization" {
			t.Fatalf("after the type was recorded: got = %+v, %v, want it on each org's record", found, err)
		}
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

func TestThePersonsRefreshTokenIsKeptPerOrgAndReplacedAtEachUse(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	_, orgA := newTestUserHome(t, accounts)
	_, orgB := newTestUserHome(t, accounts)
	for _, org := range []string{orgA, orgB} {
		if err := accounts.ConnectGitHubInstallation(ctx, &domain.OrgGitHubInstallation{OrgID: org, InstallationID: 8, AccountLogin: "carol", AccountType: "User", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}); err != nil {
			t.Fatal(err)
		}
	}
	renewal := func(access, next string, err error) (func(context.Context, string) (string, string, error), *string) {
		var handed string
		return func(_ context.Context, kept string) (string, string, error) {
			handed = kept
			return access, next, err
		}, &handed
	}

	// With none kept the renewal is handed none, and what it refuses is the answer.
	refused := errors.New("no authorisation")
	renew, handed := renewal("", "", refused)
	if access, err := accounts.RenewGitHubUserToken(ctx, orgA, 8, renew); !errors.Is(err, refused) || access != "" || *handed != "" {
		t.Fatalf("with no token kept: got = %q, %v after handing %q, want the renewal's refusal of an empty token", access, err, *handed)
	}
	if err := accounts.KeepGitHubUserToken(ctx, orgA, 8, "ghr_first"); err != nil {
		t.Fatalf("keep: %v", err)
	}
	renew, handed = renewal("ghu_access", "ghr_second", nil)
	if access, err := accounts.RenewGitHubUserToken(ctx, orgA, 8, renew); err != nil || access != "ghu_access" || *handed != "ghr_first" {
		t.Fatalf("renew: got = %q, %v after handing %q, want the access token for the one kept", access, err, *handed)
	}
	// A renewal that fails leaves the token it was handed in place.
	renew, handed = renewal("", "ghr_lost", refused)
	if _, err := accounts.RenewGitHubUserToken(ctx, orgA, 8, renew); !errors.Is(err, refused) || *handed != "ghr_second" {
		t.Fatalf("a failed renewal: got = %v after handing %q, want the token the last renewal left", err, *handed)
	}
	renew, handed = renewal("ghu_access", "ghr_third", nil)
	if _, err := accounts.RenewGitHubUserToken(ctx, orgA, 8, renew); err != nil || *handed != "ghr_second" {
		t.Fatalf("after a failed renewal: got = %v after handing %q, want the token still kept", err, *handed)
	}
	// Another org's record of the same installation holds its own, and connecting again keeps it.
	renew, handed = renewal("", "", refused)
	if _, err := accounts.RenewGitHubUserToken(ctx, orgB, 8, renew); !errors.Is(err, refused) || *handed != "" {
		t.Fatalf("the other org: handed %q, %v, want none", *handed, err)
	}
	if err := accounts.ConnectGitHubInstallation(ctx, &domain.OrgGitHubInstallation{OrgID: orgA, InstallationID: 8, AccountLogin: "carol", AccountType: "User", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}); err != nil {
		t.Fatal(err)
	}
	renew, handed = renewal("ghu_access", "ghr_fourth", nil)
	if _, err := accounts.RenewGitHubUserToken(ctx, orgA, 8, renew); err != nil || *handed != "ghr_third" {
		t.Fatalf("after connecting again: handed %q, %v, want the token kept", *handed, err)
	}
	for name, err := range map[string]error{
		"keep on an installation the org did not connect":  accounts.KeepGitHubUserToken(ctx, orgA, 99, "ghr_x"),
		"renew on an installation the org did not connect": func() error { _, err := accounts.RenewGitHubUserToken(ctx, orgA, 99, renew); return err }(),
	} {
		if !errors.Is(err, ErrGitHubInstallationNotFound) {
			t.Errorf("%s: got = %v, want = %v", name, err, ErrGitHubInstallationNotFound)
		}
	}
}

func TestTheSchemaNoLongerHoldsAProjectsRepository(t *testing.T) {
	pool := eventsTestDB(t)
	var exists bool
	if err := pool.pool.QueryRow(context.Background(), `SELECT to_regclass('project_repos') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("project_repos exists = %v, %v, want the table dropped", exists, err)
	}
}

func TestARepositorysAddressIsStoredAtCreationAndBackfilledOnceForGitHub(t *testing.T) {
	pool := eventsTestDB(t)
	ctx := context.Background()
	pes := NewPlatformExperimentsStore(pool)
	now := time.Now().UTC()
	create := func(repository, address string) string {
		t.Helper()
		pe := &domain.PlatformExperiment{
			ID: "pe-" + uuid.New().String()[:8], ProjectID: "default", Name: "with code", BudgetAcceleratorHours: 10,
			MaxAgents: 10, StartsAt: now, EndsAt: now.Add(time.Hour), Status: domain.PlatformExpOpen,
			Metrics: []domain.MetricDefinition{{Key: "val_accuracy", Direction: "maximize"}}, ReportIntervalSeconds: 30,
			Stages: []domain.Stage{{LengthPct: 100}}, CreatedAt: now, UpdatedAt: now,
			Repository: repository, RepositoryURL: address,
		}
		if err := pes.CreatePlatformExperiment(ctx, pe); err != nil {
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
	before := create("Acme/Model", "")
	bare := create("", "")
	applyTestSchema(t, pool)
	for name, tc := range map[string][2]string{
		"an address stored at creation":            {elsewhere, "https://git.example/experiments/group/model"},
		"a repository from before the column":      {before, "https://github.com/Acme/Model.git"},
		"a platform experiment with no repository": {bare, ""},
	} {
		if got := address(tc[0]); got != tc[1] {
			t.Errorf("%s: repository_url = %q, want %q", name, got, tc[1])
		}
	}
	if project, repository, err := pes.PlatformExperimentRepository(ctx, elsewhere); err != nil || project != "default" || repository != "group/model" {
		t.Fatalf("PlatformExperimentRepository = %q, %q, %v, want the project and the name", project, repository, err)
	}
}
