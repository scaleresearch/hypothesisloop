package db

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// A job whose commit declares an environment is claimed only once its environment is ready, and
// then runs in its image in place of the one it names.
func TestAJobIsClaimedOnlyOnceItsEnvironmentIsReadyAndRunsInItsImage(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	envs := NewEnvironmentsStore(w.pool)
	dockerfile := "ARG BASE\nFROM ${BASE}\nRUN echo " + w.suffix + "\n"
	key := domain.EnvironmentKey(dockerfile, "registry.test/job:1")
	want := &domain.Environment{Key: key, Base: "registry.test/job:1", Dockerfile: dockerfile, Image: "registry.test/env:" + key}
	made, err := envs.EnsureEnvironment(ctx, want)
	if err != nil || made.State != domain.EnvironmentBuilding || made.Image != want.Image {
		t.Fatalf("ensure: %+v, %v, want a building environment", made, err)
	}
	if err := envs.FinishEnvironment(ctx, key, domain.EnvironmentFailed, "no"); err != nil {
		t.Fatal(err)
	}
	if again, err := envs.EnsureEnvironment(ctx, &domain.Environment{Key: key, Base: "other", Dockerfile: "other", Image: "other"}); err != nil || again.State != domain.EnvironmentFailed || again.Base != want.Base {
		t.Fatalf("ensure again: %+v, %v, want the stored row unchanged", again, err)
	}
	// A failed build is final: only a building row takes an outcome.
	if err := envs.FinishEnvironment(ctx, key, domain.EnvironmentReady, ""); err != nil {
		t.Fatal(err)
	}
	if stored, _ := envs.GetEnvironment(ctx, key); stored.State != domain.EnvironmentFailed {
		t.Fatalf("a failed environment became %s", stored.State)
	}
	if _, err := w.pool.pool.Exec(ctx, `UPDATE environments SET state = 'building' WHERE key = $1`, key); err != nil {
		t.Fatal(err)
	}

	id := w.queued(t, "env-job", w.peA)
	if _, err := w.pool.pool.Exec(ctx, `UPDATE experiments SET environment_key = $2 WHERE id = $1`, id, key); err != nil {
		t.Fatal(err)
	}
	if got := w.get(t, id).EnvironmentKey; got != key {
		t.Fatalf("environment_key read back %q, want %q", got, key)
	}
	if w.claim(t, id, w.ofA) {
		t.Fatal("a job was placed while its environment builds")
	}
	if building, err := envs.ListBuildingEnvironments(ctx, 0); err != nil || !containsEnvironment(building, key) {
		t.Fatalf("building: %v, want %s among them", err, key)
	}
	if err := envs.FinishEnvironment(ctx, key, domain.EnvironmentReady, ""); err != nil {
		t.Fatal(err)
	}
	resolved := &domain.JobSpec{Image: "registry.test/job:1", AcceleratorCount: 1}
	claimed, err := w.exps.ClaimSubmitted(ctx, id, w.ofA, resolved, nil)
	if err != nil || !claimed || resolved.Image != want.Image {
		t.Fatalf("claim once ready: %v, %v, image %q, want it placed in %q", claimed, err, resolved.Image, want.Image)
	}
	if stored := w.get(t, id); stored.ResolvedJob == nil || stored.ResolvedJob.Image != want.Image || stored.Job.Image != "registry.test/job:1" {
		t.Errorf("stored image %+v and job image %q, want the environment's resolved and the named one kept", stored.ResolvedJob, stored.Job.Image)
	}
}

// A build the platform failed waits retryAfter before it is listed again, the last attempt
// abandons the environment, and ensuring it again makes it building from the first attempt.
func TestAnEnvironmentThePlatformFailsIsRetriedThenAbandonedAndEnsuredAgain(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	envs := NewEnvironmentsStore(w.pool)
	dockerfile := "ARG BASE\nFROM ${BASE}\nRUN echo " + w.suffix + " retried\n"
	key := domain.EnvironmentKey(dockerfile, "registry.test/job:1")
	env := &domain.Environment{Key: key, Base: "registry.test/job:1", Dockerfile: dockerfile, Image: "registry.test/env:" + key}
	if _, err := envs.EnsureEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	listed := func(retryAfter time.Duration) bool {
		building, err := envs.ListBuildingEnvironments(ctx, retryAfter)
		if err != nil {
			t.Fatal(err)
		}
		return containsEnvironment(building, key)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		abandoned, err := envs.FailEnvironmentAttempt(ctx, key, 3)
		if err != nil || abandoned != (attempt == 3) {
			t.Fatalf("attempt %d: abandoned %v, %v", attempt, abandoned, err)
		}
		if attempt < 3 && (listed(time.Hour) || !listed(0)) {
			t.Fatalf("attempt %d: listed within the hour %v, want held back until retryAfter passed", attempt, listed(time.Hour))
		}
	}
	if stored, _ := envs.GetEnvironment(ctx, key); stored.State != domain.EnvironmentAbandoned || stored.Attempts != 3 || listed(0) {
		t.Fatalf("after the last attempt: %+v, want it abandoned and not listed", stored)
	}
	again, err := envs.EnsureEnvironment(ctx, env)
	if err != nil || again.State != domain.EnvironmentBuilding || again.Attempts != 0 || !listed(time.Hour) {
		t.Fatalf("ensured again: %+v, %v, want it building from the first attempt", again, err)
	}
}

func containsEnvironment(envs []*domain.Environment, key string) bool {
	for _, env := range envs {
		if env.Key == key {
			return true
		}
	}
	return false
}
