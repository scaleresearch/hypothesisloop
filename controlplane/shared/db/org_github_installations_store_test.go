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

	first := &domain.OrgGitHubInstallation{OrgID: orgA, InstallationID: 7, AccountLogin: "Acme", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}
	if err := accounts.ConnectGitHubInstallation(ctx, first); err != nil || first.ConnectedAt.IsZero() {
		t.Fatalf("connect: got = %v at %v, want = nil and the moment it happened", err, first.ConnectedAt)
	}
	for _, more := range []*domain.OrgGitHubInstallation{
		{OrgID: orgA, InstallationID: 8, AccountLogin: "rival", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}},
		// The same installation serves a second org, each with a row of its own.
		{OrgID: orgB, InstallationID: 7, AccountLogin: "Acme", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-2"}},
	} {
		if err := accounts.ConnectGitHubInstallation(ctx, more); err != nil {
			t.Fatalf("connect %+v: %v", more, err)
		}
	}

	// Connected again, it keeps who connected it and when, and takes the account's name of today.
	again := &domain.OrgGitHubInstallation{OrgID: orgA, InstallationID: 7, AccountLogin: "Acme-Renamed", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-9"}}
	if err := accounts.ConnectGitHubInstallation(ctx, again); err != nil {
		t.Fatalf("connect again: %v", err)
	}
	if again.ConnectedActor.ID != "user-1" || !again.ConnectedAt.Equal(first.ConnectedAt) {
		t.Fatalf("connected again = %+v, want who and when of the first connect (%s at %s)", again, first.ConnectedActor.ID, first.ConnectedAt.Format(time.RFC3339Nano))
	}

	listed, err := accounts.ListGitHubInstallations(ctx, orgA)
	if err != nil || len(listed) != 2 || listed[0].InstallationID != 7 || listed[0].AccountLogin != "Acme-Renamed" || listed[1].InstallationID != 8 {
		t.Fatalf("list: got = %+v, %v, want the org's two, oldest first, the first under its new name", listed, err)
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

	if err := accounts.ConnectGitHubInstallation(ctx, &domain.OrgGitHubInstallation{OrgID: "org-nowhere", InstallationID: 7, AccountLogin: "Acme", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("connect to an org that does not exist: got = %v, want = %v", err, ErrOrgNotFound)
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
