package environments

import (
	"context"
	"errors"
	"testing"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
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
	if _, err := svc.ensure(ctx, "org-a", "FROM ubuntu:24.04\n", "registry.test/hl/job:1"); !errors.As(err, &invalid) {
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
