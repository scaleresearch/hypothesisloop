package db

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

	// A row from before the type was kept reads with none, and connecting it again records it.
	if _, err := pool.pool.Exec(ctx, `UPDATE org_github_installations SET account_type = '' WHERE org_id = $1 AND installation_id = 7`, orgA); err != nil {
		t.Fatal(err)
	}
	applyTestSchema(t, pool)
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

const testSigningSecret = "a signing secret of a test, long enough"

func newTestInstallationsStore(t *testing.T, pool *Pool, secret string) *GitHubInstallationsStore {
	t.Helper()
	store, err := NewGitHubInstallationsStore(NewAccountsStore(pool), secret)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestThePersonsAuthorisationIsKeptPerOrgAndReplacedAtEachUse(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := newTestInstallationsStore(t, pool, testSigningSecret)
	ctx := context.Background()
	_, orgA := newTestUserHome(t, accounts.AccountsStore)
	_, orgB := newTestUserHome(t, accounts.AccountsStore)
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
		t.Fatalf("with none kept: got = %q, %v after handing %q, want the renewal's refusal of an empty one", access, err, *handed)
	}
	if err := accounts.KeepGitHubUserAuthorization(ctx, orgA, 8, ""); err == nil {
		t.Fatal("an empty authorisation was kept")
	}
	if err := accounts.KeepGitHubUserAuthorization(ctx, orgA, 8, "ghr_first"); err != nil {
		t.Fatalf("keep: %v", err)
	}
	renew, handed = renewal("ghu_access", "ghr_second", nil)
	if access, err := accounts.RenewGitHubUserToken(ctx, orgA, 8, renew); err != nil || access != "ghu_access" || *handed != "ghr_first" {
		t.Fatalf("renew: got = %q, %v after handing %q, want the access token for the one kept", access, err, *handed)
	}
	// A renewal that fails leaves the one it was handed in place, and one that answers nothing
	// to keep is a failure too.
	renew, handed = renewal("", "ghr_lost", refused)
	if _, err := accounts.RenewGitHubUserToken(ctx, orgA, 8, renew); !errors.Is(err, refused) || *handed != "ghr_second" {
		t.Fatalf("a failed renewal: got = %v after handing %q, want the one the last renewal left", err, *handed)
	}
	renew, _ = renewal("ghu_access", "", nil)
	if _, err := accounts.RenewGitHubUserToken(ctx, orgA, 8, renew); err == nil {
		t.Fatal("a renewal that answered nothing to keep went through")
	}
	renew, handed = renewal("ghu_access", "ghr_third", nil)
	if _, err := accounts.RenewGitHubUserToken(ctx, orgA, 8, renew); err != nil || *handed != "ghr_second" {
		t.Fatalf("after failed renewals: got = %v after handing %q, want the one still kept", err, *handed)
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
		t.Fatalf("after connecting again: handed %q, %v, want the one kept", *handed, err)
	}
	for name, err := range map[string]error{
		"keep on an installation the org did not connect":  accounts.KeepGitHubUserAuthorization(ctx, orgA, 99, "ghr_x"),
		"renew on an installation the org did not connect": func() error { _, err := accounts.RenewGitHubUserToken(ctx, orgA, 99, renew); return err }(),
	} {
		if !errors.Is(err, ErrGitHubInstallationNotFound) {
			t.Errorf("%s: got = %v, want = %v", name, err, ErrGitHubInstallationNotFound)
		}
	}
}

// GitHub replaces a refresh token the moment it reads it, so a caller that goes away while it is
// being renewed must not leave the spent one stored.
func TestARenewalIsKeptThoughTheRequestThatAskedForItWasCancelled(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := newTestInstallationsStore(t, pool, testSigningSecret)
	_, org := newTestUserHome(t, accounts.AccountsStore)
	background := context.Background()
	if err := accounts.ConnectGitHubInstallation(background, &domain.OrgGitHubInstallation{OrgID: org, InstallationID: 8, AccountLogin: "carol", AccountType: "User", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.KeepGitHubUserAuthorization(background, org, 8, "ghr_first"); err != nil {
		t.Fatal(err)
	}
	request, disconnect := context.WithCancel(background)
	access, err := accounts.RenewGitHubUserToken(request, org, 8, func(ctx context.Context, kept string) (string, string, error) {
		disconnect()
		if ctx.Err() != nil {
			t.Error("the renewal was cancelled with the request")
		}
		return "ghu_access", "ghr_second", nil
	})
	if err != nil || access != "ghu_access" {
		t.Fatalf("a renewal whose request was cancelled while GitHub answered: got = %q, %v, want it kept and answered", access, err)
	}
	var handed string
	if _, err := accounts.RenewGitHubUserToken(background, org, 8, func(_ context.Context, kept string) (string, string, error) {
		handed = kept
		return "ghu_access", "ghr_third", nil
	}); err != nil || handed != "ghr_second" {
		t.Fatalf("the next renewal was handed %q, %v, want the one GitHub issued while the request was cancelled", handed, err)
	}
}

// What is kept acts as a person on GitHub, so the table holds it sealed: bound to its row, and
// unreadable under another secret.
func TestThePersonsAuthorisationIsStoredSealed(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := newTestInstallationsStore(t, pool, testSigningSecret)
	ctx := context.Background()
	_, org := newTestUserHome(t, accounts.AccountsStore)
	for _, id := range []int64{8, 9} {
		if err := accounts.ConnectGitHubInstallation(ctx, &domain.OrgGitHubInstallation{OrgID: org, InstallationID: id, AccountLogin: fmt.Sprintf("carol-%d", id), AccountType: "User", ConnectedActor: domain.Actor{Kind: domain.ActorUser, ID: "user-1"}}); err != nil {
			t.Fatal(err)
		}
	}
	stored := func(id int64) []byte {
		t.Helper()
		var kept []byte
		if err := pool.pool.QueryRow(ctx, `SELECT user_authorization FROM org_github_installations WHERE org_id = $1 AND installation_id = $2`, org, id).Scan(&kept); err != nil {
			t.Fatal(err)
		}
		return kept
	}
	if stored(8) != nil {
		t.Fatalf("an installation with none kept stores %x, want NULL", stored(8))
	}
	const secret = "refresh:ghr_the_persons_own"
	if err := accounts.KeepGitHubUserAuthorization(ctx, org, 8, secret); err != nil {
		t.Fatal(err)
	}
	first := stored(8)
	if bytes.Contains(first, []byte("ghr_")) || len(first) <= len(secret) {
		t.Fatalf("the column holds %q, want it sealed", first)
	}
	if err := accounts.KeepGitHubUserAuthorization(ctx, org, 8, secret); err != nil || bytes.Equal(stored(8), first) {
		t.Fatalf("kept twice: %v, stored the same bytes %v, want a fresh nonce each time", err, bytes.Equal(stored(8), first))
	}
	renew := func(_ context.Context, kept string) (string, string, error) { return "ghu_access", kept, nil }
	// Moved to another row, or read under another secret, it does not open.
	if _, err := pool.pool.Exec(ctx, `UPDATE org_github_installations SET user_authorization = $2 WHERE org_id = $1 AND installation_id = 9`, org, stored(8)); err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.RenewGitHubUserToken(ctx, org, 9, renew); !errors.Is(err, ErrGitHubAuthorizationUnreadable) {
		t.Fatalf("an authorisation copied to another row: got = %v, want = %v", err, ErrGitHubAuthorizationUnreadable)
	}
	rotated := newTestInstallationsStore(t, pool, "another signing secret, as after a rotation")
	if _, err := rotated.RenewGitHubUserToken(ctx, org, 8, renew); !errors.Is(err, ErrGitHubAuthorizationUnreadable) {
		t.Fatalf("under another secret: got = %v, want = %v", err, ErrGitHubAuthorizationUnreadable)
	}
	if access, err := accounts.RenewGitHubUserToken(ctx, org, 8, renew); err != nil || access != "ghu_access" {
		t.Fatalf("under its own secret: got = %q, %v, want it opened", access, err)
	}
	// Connecting again under the new secret replaces what could not be read.
	if err := rotated.KeepGitHubUserAuthorization(ctx, org, 8, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.RenewGitHubUserToken(ctx, org, 8, renew); err != nil {
		t.Fatalf("after keeping again under the new secret: %v", err)
	}
	var clear bool
	if err := pool.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'org_github_installations' AND column_name = 'user_refresh_token')`).Scan(&clear); err != nil || clear {
		t.Fatalf("the column that held refresh tokens in the clear exists = %v, %v, want it dropped", clear, err)
	}
	if _, err := NewGitHubInstallationsStore(NewAccountsStore(pool), ""); err == nil {
		t.Fatal("a store was built with no secret to seal under")
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
