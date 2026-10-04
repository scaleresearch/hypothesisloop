package environments

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/githost"
)

type memoryStore struct {
	mu   sync.Mutex
	envs map[string]*domain.Environment
}

func (s *memoryStore) EnsureEnvironment(_ context.Context, env *domain.Environment) (*domain.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stored, ok := s.envs[env.Key]; ok {
		if stored.State == domain.EnvironmentAbandoned {
			stored.State, stored.Attempts, stored.UpdatedAt = domain.EnvironmentBuilding, 0, time.Now()
		}
		return stored, nil
	}
	made := *env
	made.State, made.UpdatedAt = domain.EnvironmentBuilding, time.Now()
	s.envs[env.Key] = &made
	return &made, nil
}

func (s *memoryStore) GetEnvironment(_ context.Context, key string) (*domain.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.envs[key], nil
}

func (s *memoryStore) ListBuildingEnvironments(_ context.Context, retryAfter time.Duration) ([]*domain.Environment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*domain.Environment
	for _, env := range s.envs {
		if env.State == domain.EnvironmentBuilding && (env.Attempts == 0 || !env.UpdatedAt.After(time.Now().Add(-retryAfter))) {
			copied := *env
			out = append(out, &copied)
		}
	}
	return out, nil
}

func (s *memoryStore) FinishEnvironment(_ context.Context, key string, state domain.EnvironmentState, logTail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if env := s.envs[key]; env != nil && env.State == domain.EnvironmentBuilding {
		env.State, env.LogTail = state, logTail
	}
	return nil
}

func (s *memoryStore) FailEnvironmentAttempt(_ context.Context, key string, maxAttempts int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	env := s.envs[key]
	if env == nil || env.State != domain.EnvironmentBuilding {
		return false, nil
	}
	env.Attempts++
	env.UpdatedAt = time.Now()
	if env.Attempts >= maxAttempts {
		env.State = domain.EnvironmentAbandoned
	}
	return env.State == domain.EnvironmentAbandoned, nil
}

func (s *memoryStore) state(key string) domain.EnvironmentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.envs[key].State
}

// due makes the build the platform failed last of key due again.
func (s *memoryStore) due(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envs[key].UpdatedAt = time.Now().Add(-retryAfter)
}

// fileHost holds files by ref.
type fileHost struct {
	githost.Provider
	files map[string]string
	refs  []string
}

func (h *fileHost) FileAt(_ context.Context, _, _, ref, path string) ([]byte, error) {
	h.refs = append(h.refs, ref)
	if file, ok := h.files[ref]; ok && path == domain.EnvironmentFile {
		return []byte(file), nil
	}
	return nil, githost.ErrFileNotFound
}

const rust = "# the toolchain\nARG BASE\nFROM ${BASE}\nRUN apt-get install -y rustc\n"

func TestTheSameFileOnTheSameBaseIsOneEnvironmentAndAnotherBaseAnother(t *testing.T) {
	store := &memoryStore{envs: map[string]*domain.Environment{}}
	svc := NewService(store, &fileHost{files: map[string]string{"c1": rust, "c2": rust}}, "registry.test/hl/hypothesisloop-environment")
	ctx := context.Background()
	first, err := svc.Declared(ctx, "prj", "acme/model", "c1", "registry.test/hl/job:1")
	if err != nil || first == nil || first.State != domain.EnvironmentBuilding || first.Image != "registry.test/hl/hypothesisloop-environment:"+first.Key {
		t.Fatalf("first: %+v, %v, want a building environment pushed under its key", first, err)
	}
	again, _ := svc.Declared(ctx, "prj", "acme/model", "c2", "registry.test/hl/job:1")
	other, _ := svc.Declared(ctx, "prj", "acme/model", "c2", "registry.test/hl/job-cuda:1")
	if again.Key != first.Key || other.Key == first.Key || len(store.envs) != 2 {
		t.Errorf("keys %s, %s, %s with %d stored, want one per file and base", first.Key, again.Key, other.Key, len(store.envs))
	}
	if none, err := svc.Declared(ctx, "prj", "acme/model", "c3", "registry.test/hl/job:1"); none != nil || err != nil {
		t.Errorf("a commit without the file: %+v, %v, want none", none, err)
	}
	var invalid *domain.EnvironmentFileError
	if _, err := svc.Ensure(ctx, "FROM ubuntu:24.04\n", "registry.test/hl/job:1"); !errors.As(err, &invalid) {
		t.Errorf("a file on its own base: %v, want an EnvironmentFileError", err)
	}
}

// builds answers each build by its Dockerfile: built, failed with output, or the platform failing
// it while down. pulled records the images pulled, and a pull of an image in refused fails.
type builds struct {
	mu      sync.Mutex
	down    bool
	asked   map[string]int
	pulled  []string
	refused map[string]bool
}

func (b *builds) Build(_ context.Context, dockerfile, base, image string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked[dockerfile]++
	switch {
	case strings.Contains(dockerfile, "nonexistent"):
		return &BuildFailed{Log: "#5 RUN apt-get install -y nonexistent\nE: Unable to locate package nonexistent\n"}
	case b.down:
		return errors.New("failed to solve: failed to get frontend inputs: invalid nil input definition to definition op")
	}
	return nil
}

func (b *builds) Pull(_ context.Context, image string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pulled = append(b.pulled, image)
	if b.refused[image] {
		return errors.New("unauthorized")
	}
	return nil
}

func passes(ctx context.Context, loop *Loop) {
	var wg sync.WaitGroup
	loop.pass(ctx, &wg)
	wg.Wait()
}

// A file that fails is final. A build the platform fails waits and is tried again, is abandoned
// after the last attempt, and is built again once a submit names it again.
func TestTheBuilderBlamesTheFileOnlyForItsOwnFailureAndGivesUpOnThePlatformsAfterItsAttempts(t *testing.T) {
	store := &memoryStore{envs: map[string]*domain.Environment{}}
	svc := NewService(store, &fileHost{}, "registry.test/env")
	ctx := context.Background()
	good, _ := svc.Ensure(ctx, rust, "base:1")
	bad, _ := svc.Ensure(ctx, "ARG BASE\nFROM ${BASE}\nRUN apt-get install -y nonexistent\n", "base:1")
	builder := &builds{down: true, asked: map[string]int{}}
	loop := NewLoop(store, builder, []string{"base:1"}, zap.NewNop())

	passes(ctx, loop)
	if store.state(good.Key) != domain.EnvironmentBuilding || store.state(bad.Key) != domain.EnvironmentFailed {
		t.Fatalf("with the platform failing: %s and %s, want the good one still building and the bad one failed", store.state(good.Key), store.state(bad.Key))
	}
	if tail := store.envs[bad.Key].LogTail; !strings.Contains(tail, "Unable to locate package nonexistent") {
		t.Errorf("the failed build keeps %q, want the end of its output", tail)
	}
	passes(ctx, loop)
	if builder.asked[asRoot(rust)] != 1 {
		t.Fatalf("asked %d times, want the build the platform failed held back", builder.asked[asRoot(rust)])
	}
	for attempt := 2; attempt <= maxAttempts; attempt++ {
		store.due(good.Key)
		passes(ctx, loop)
	}
	if store.state(good.Key) != domain.EnvironmentAbandoned || builder.asked[asRoot(rust)] != maxAttempts {
		t.Fatalf("after %d builds the platform failed: %s, want it abandoned", builder.asked[asRoot(rust)], store.state(good.Key))
	}
	store.due(good.Key)
	passes(ctx, loop)
	if builder.asked[asRoot(rust)] != maxAttempts {
		t.Fatalf("an abandoned environment was built again by the loop alone")
	}

	builder.down = false
	if again, err := svc.Ensure(ctx, rust, "base:1"); err != nil || again.State != domain.EnvironmentBuilding || again.Attempts != 0 {
		t.Fatalf("a submit naming the abandoned environment: %+v, %v, want it building again", again, err)
	}
	if failed, _ := svc.Ensure(ctx, "ARG BASE\nFROM ${BASE}\nRUN apt-get install -y nonexistent\n", "base:1"); failed.State != domain.EnvironmentFailed {
		t.Errorf("a submit naming the failed file: %s, want it failed still", failed.State)
	}
	passes(ctx, loop)
	if store.state(good.Key) != domain.EnvironmentReady || builder.asked[asRoot(rust)] != maxAttempts+1 {
		t.Errorf("rebuilt: %s after %d builds, want ready", store.state(good.Key), builder.asked[asRoot(rust)])
	}
}

// The loop pulls every base into the builder's cache as it starts, past one that fails.
func TestTheLoopWarmsTheBuildersCacheWithEveryBase(t *testing.T) {
	builder := &builds{asked: map[string]int{}, refused: map[string]bool{"registry.test/job:1": true}}
	loop := NewLoop(&memoryStore{envs: map[string]*domain.Environment{}}, builder, []string{"registry.test/agent:1", "registry.test/job:1", "registry.test/job-tt:1"}, zap.NewNop())
	loop.warm(context.Background())
	if want := []string{"registry.test/agent:1", "registry.test/job:1", "registry.test/job-tt:1"}; !slices.Equal(builder.pulled, want) {
		t.Errorf("pulled %v, want %v", builder.pulled, want)
	}
}

func TestOnlyTheEndOfALongBuildOutputIsKept(t *testing.T) {
	log := strings.Repeat("compiling a crate\n", 2000) + "error[E0425]: cannot find value\n"
	tail := LastBytes(log, domain.MaxEnvironmentLogBytes)
	if len(tail) > domain.MaxEnvironmentLogBytes || !strings.HasSuffix(tail, "error[E0425]: cannot find value\n") || !strings.HasPrefix(tail, "compiling") {
		t.Errorf("tail of %d bytes, want at most %d starting on a line and ending with the error", len(tail), domain.MaxEnvironmentLogBytes)
	}
}
