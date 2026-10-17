package environments

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/clients/githost"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// memoryStore holds environments by key, and the orgs a cluster builds for.
type memoryStore struct {
	envs     map[string]*domain.Environment
	builders map[string]bool
}

func (s *memoryStore) OrgHasEnvironmentBuilder(_ context.Context, orgID string) (bool, error) {
	return s.builders[orgID], nil
}

func (s *memoryStore) EnsureEnvironment(_ context.Context, env *domain.Environment) (*domain.Environment, error) {
	if stored, ok := s.envs[env.Key]; ok {
		return stored, nil
	}
	made := *env
	made.State = domain.EnvironmentBuilding
	s.envs[env.Key] = &made
	return &made, nil
}

func (s *memoryStore) GetEnvironment(_ context.Context, key string) (*domain.Environment, error) {
	return s.envs[key], nil
}

// fileHost holds files by ref.
type fileHost struct {
	githost.Provider
	files map[string]string
}

func (h *fileHost) FileAt(_ context.Context, _, _, ref, path string) ([]byte, error) {
	if file, ok := h.files[ref]; ok && path == domain.EnvironmentFile {
		return []byte(file), nil
	}
	return nil, githost.ErrFileNotFound
}

const rust = "# the toolchain\nARG BASE\nFROM ${BASE}\nRUN apt-get install -y rustc\n"

func TestTheSameFileOnTheSameBaseIsOneEnvironmentOfAnOrgAndAnotherBaseOrOrgAnother(t *testing.T) {
	store := &memoryStore{envs: map[string]*domain.Environment{}, builders: map[string]bool{"org-a": true, "org-b": true}}
	svc := NewService(store, &fileHost{files: map[string]string{"c1": rust, "c2": rust}})
	ctx := context.Background()
	first, err := svc.Declared(ctx, "org-a", "prj", "acme/model", "c1", "registry.test/hl/job:1")
	if err != nil || first == nil || first.State != domain.EnvironmentBuilding || first.OrgID != "org-a" || first.Image != "" {
		t.Fatalf("first: %+v, %v, want a building environment of org-a with no image yet", first, err)
	}
	again, _ := svc.Declared(ctx, "org-a", "prj", "acme/model", "c2", "registry.test/hl/job:1")
	otherBase, _ := svc.Declared(ctx, "org-a", "prj", "acme/model", "c2", "registry.test/hl/job-cuda:1")
	otherOrg, _ := svc.Declared(ctx, "org-b", "prj", "acme/model", "c2", "registry.test/hl/job:1")
	if again.Key != first.Key || otherBase.Key == first.Key || otherOrg.Key == first.Key || len(store.envs) != 3 {
		t.Errorf("keys %s, %s, %s, %s with %d stored, want one per org, file and base", first.Key, again.Key, otherBase.Key, otherOrg.Key, len(store.envs))
	}
	if none, err := svc.Declared(ctx, "org-a", "prj", "acme/model", "c3", "registry.test/hl/job:1"); none != nil || err != nil {
		t.Errorf("a commit without the file: %+v, %v, want none", none, err)
	}
	var invalid *domain.EnvironmentFileError
	if _, err := svc.ensure(ctx, "org-a", "project-a", "owner/repo", "FROM ubuntu:24.04\n", "registry.test/hl/job:1"); !errors.As(err, &invalid) {
		t.Errorf("a file on its own base: %v, want an EnvironmentFileError", err)
	}
}

func TestAnEnvironmentIsRefusedToAnOrgNoClusterBuildsFor(t *testing.T) {
	store := &memoryStore{envs: map[string]*domain.Environment{}, builders: map[string]bool{"org-a": true}}
	svc := NewService(store, &fileHost{files: map[string]string{"c1": rust}})
	if _, err := svc.Declared(context.Background(), "org-b", "prj", "acme/model", "c1", "registry.test/hl/job:1"); !errors.Is(err, domain.ErrNoEnvironmentBuilder) || len(store.envs) != 0 {
		t.Fatalf("got %v with %d stored, want ErrNoEnvironmentBuilder and nothing stored", err, len(store.envs))
	}
}

// digests names each reference by the digest its registry holds it under, and none it lacks.
type digests map[string]string

func (d digests) Pinned(_ context.Context, ref string) (string, error) {
	repository, _, _ := strings.Cut(ref, ":")
	if sum, ok := d[ref]; ok {
		return repository + "@" + sum, nil
	}
	return "", errors.New("manifest unknown")
}

// An environment of an offered job image is built on its base and takes the tools from the image,
// each named by its digest: a release under a new tag whose images are the same is the same
// environment, and one whose tools alone changed keeps the base its layers were built on.
func TestAnEnvironmentIsKeyedOnTheDigestsOfItsBaseAndToolsNotOnTheirTags(t *testing.T) {
	store := &memoryStore{envs: map[string]*domain.Environment{}, builders: map[string]bool{"org-a": true}}
	host := &fileHost{files: map[string]string{"c1": rust}}
	registry := digests{"registry.test/hl/job-base:7": "sha256:base", "registry.test/hl/job-base:8": "sha256:base", "registry.test/hl/job-base:9": "sha256:base",
		"registry.test/hl/job:7": "sha256:tools-1", "registry.test/hl/job:8": "sha256:tools-1", "registry.test/hl/job:9": "sha256:tools-2"}
	release := func(tag string) *Service {
		offered := []domain.JobImage{{Name: "python", Image: "registry.test/hl/job:" + tag, EnvironmentBase: "registry.test/hl/job-base:" + tag}}
		return NewService(store, host).WithJobImages(offered, registry)
	}
	ctx := context.Background()
	first, err := release("7").Declared(ctx, "org-a", "prj", "acme/model", "c1", "registry.test/hl/job:7")
	if err != nil {
		t.Fatal(err)
	}
	if first.Base != "registry.test/hl/job-base@sha256:base" || first.Tools != "registry.test/hl/job@sha256:tools-1" {
		t.Errorf("built on %q with tools of %q, want the base and the image by digest", first.Base, first.Tools)
	}
	retagged, _ := release("8").Declared(ctx, "org-a", "prj", "acme/model", "c1", "registry.test/hl/job:8")
	newTools, _ := release("9").Declared(ctx, "org-a", "prj", "acme/model", "c1", "registry.test/hl/job:9")
	if retagged.Key != first.Key || newTools.Key == first.Key || newTools.Base != first.Base {
		t.Errorf("keys %s, %s, %s on bases %s and %s, want a retag the same and new tools another on the same base", first.Key, retagged.Key, newTools.Key, first.Base, newTools.Base)
	}

	other, err := release("7").Declared(ctx, "org-a", "prj", "acme/model", "c1", "registry.test/team/own:1")
	if err == nil || other != nil {
		t.Fatalf("an image its registry does not name: %+v, %v", other, err)
	}
	var unresolved *domain.ImageUnresolvedError
	if !errors.As(err, &unresolved) || unresolved.Image != "registry.test/team/own:1" || len(store.envs) != 2 {
		t.Errorf("got %v with %d stored, want an ImageUnresolvedError naming the image and nothing more stored", err, len(store.envs))
	}
	registry["registry.test/team/own:1"] = "sha256:own"
	own, err := release("7").Declared(ctx, "org-a", "prj", "acme/model", "c1", "registry.test/team/own:1")
	if err != nil || own.Base != "registry.test/team/own@sha256:own" || own.Tools != "" {
		t.Errorf("an image offered by nobody: %+v, %v, want it built on itself by digest with no tools", own, err)
	}
}
