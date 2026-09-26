package db

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

func TestAProjectHoldsOneRepositoryAndSharesAnInstallationWithOthers(t *testing.T) {
	pool := eventsTestDB(t)
	accounts := NewAccountsStore(pool)
	ctx := context.Background()
	_, orgID := newTestUserHome(t, accounts)
	first := defaultProjectOf(t, accounts, orgID)
	second, err := accounts.CreateProject(ctx, orgID, "second", 0)
	if err != nil {
		t.Fatalf("create project: got = %v, want = nil", err)
	}

	if repo, err := accounts.GetProjectRepo(ctx, first.ID); err != nil || repo != nil {
		t.Fatalf("repo of a new project: got = %+v, %v, want none", repo, err)
	}
	if err := accounts.DetachProjectRepo(ctx, first.ID); !errors.Is(err, ErrProjectRepoNotFound) {
		t.Fatalf("detach with none attached: got = %v, want = %v", err, ErrProjectRepoNotFound)
	}

	attached := &domain.ProjectRepo{ProjectID: first.ID, InstallationID: 7, RepositoryID: 11, Repo: "acme/model"}
	if err := accounts.AttachProjectRepo(ctx, attached); err != nil {
		t.Fatalf("attach: got = %v, want = nil", err)
	}
	if attached.AttachedAt.IsZero() {
		t.Fatal("attach did not report when it happened")
	}
	// Two projects on one installation, each with its own repository.
	if err := accounts.AttachProjectRepo(ctx, &domain.ProjectRepo{ProjectID: second.ID, InstallationID: 7, RepositoryID: 12, Repo: "acme/other"}); err != nil {
		t.Fatalf("attach on the same installation: got = %v, want = nil", err)
	}
	got, err := accounts.GetProjectRepo(ctx, first.ID)
	if err != nil || got == nil || *got != *attached {
		t.Fatalf("read back: got = %+v, %v, want = %+v", got, err, attached)
	}

	replaced := &domain.ProjectRepo{ProjectID: first.ID, InstallationID: 8, RepositoryID: 13, Repo: "acme/renamed"}
	if err := accounts.AttachProjectRepo(ctx, replaced); err != nil {
		t.Fatalf("attach over an attached repo: got = %v, want = nil", err)
	}
	got, err = accounts.GetProjectRepo(ctx, first.ID)
	if err != nil || got == nil || got.RepositoryID != 13 || got.InstallationID != 8 || got.Repo != "acme/renamed" {
		t.Fatalf("after a second attach: got = %+v, %v, want the new repository alone", got, err)
	}

	if err := accounts.DetachProjectRepo(ctx, first.ID); err != nil {
		t.Fatalf("detach: got = %v, want = nil", err)
	}
	if repo, err := accounts.GetProjectRepo(ctx, first.ID); err != nil || repo != nil {
		t.Fatalf("after detach: got = %+v, %v, want none", repo, err)
	}
	if other, err := accounts.GetProjectRepo(ctx, second.ID); err != nil || other == nil || other.RepositoryID != 12 {
		t.Fatalf("the other project after a detach: got = %+v, %v, want its repository untouched", other, err)
	}

	if err := accounts.AttachProjectRepo(ctx, &domain.ProjectRepo{ProjectID: "prj_missing", InstallationID: 7, RepositoryID: 11, Repo: "acme/model"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("attach to a project that does not exist: got = %v, want = %v", err, ErrProjectNotFound)
	}
}
