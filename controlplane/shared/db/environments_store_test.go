package db

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// environments is a world of two orgs whose clusters build, and a way to make environments in it.
type environments struct {
	*twoOrgs
	envs *EnvironmentsStore
}

func newEnvironments(t *testing.T) *environments {
	t.Helper()
	w := &environments{twoOrgs: newTwoOrgs(t)}
	w.envs = NewEnvironmentsStore(w.pool)
	for _, name := range []string{w.ofA, w.ofB} {
		if err := w.clusters.RecordBuilds(context.Background(), name, true); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w *environments) ensure(t *testing.T, org, what string) *domain.Environment {
	t.Helper()
	dockerfile := "ARG BASE\nFROM ${BASE}\nRUN echo " + w.suffix + " " + what + "\n"
	env, err := w.envs.EnsureEnvironment(context.Background(), &domain.Environment{Key: domain.EnvironmentKey(org, dockerfile, "registry.test/job:1"), OrgID: org, Base: "registry.test/job:1", Dockerfile: dockerfile})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (w *environments) assign(t *testing.T, key string, live ...string) *domain.Environment {
	t.Helper()
	env, err := w.envs.AssignEnvironment(context.Background(), key, live, domain.EnvironmentBuildLease)
	if err != nil || env == nil {
		t.Fatalf("assign %s: %+v, %v", key, env, err)
	}
	return env
}

func (w *environments) finish(t *testing.T, cluster string, env *domain.Environment, outcome domain.EnvironmentBuildOutcome) bool {
	t.Helper()
	took, err := w.envs.FinishEnvironmentBuild(context.Background(), cluster, env.Key, env.Attempt, outcome)
	if err != nil {
		t.Fatal(err)
	}
	return took
}

// A job whose commit declares an environment is claimed only once its environment is ready, and
// then runs in the image its cluster pushed in place of the one it names.
func TestAJobIsClaimedOnlyOnceItsEnvironmentIsReadyAndRunsInItsImage(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	made := w.ensure(t, w.orgA, "claimed")
	if made.State != domain.EnvironmentBuilding || made.Image != "" || made.OrgID != w.orgA || made.ClusterName != "" || made.Attempt != 0 {
		t.Fatalf("ensure: %+v, want a building environment of org a assigned to none", made)
	}
	id := w.queued(t, "env-job", w.peA)
	if _, err := w.pool.pool.Exec(ctx, `UPDATE experiments SET environment_key = $2 WHERE id = $1`, id, made.Key); err != nil {
		t.Fatal(err)
	}
	if w.claim(t, id, w.ofA) {
		t.Fatal("a job was placed while its environment builds")
	}
	env := w.assign(t, made.Key, w.ofA)
	image := "registry.test/env@sha256:" + w.suffix
	if !w.finish(t, w.ofA, env, domain.EnvironmentBuildOutcome{Image: image}) {
		t.Fatal("the assigned cluster's report was refused")
	}
	resolved := &domain.JobSpec{Image: "registry.test/job:1", AcceleratorCount: 1}
	claimed, err := w.exps.ClaimSubmitted(ctx, id, w.ofA, resolved, nil)
	if err != nil || !claimed || resolved.Image != image {
		t.Fatalf("claim once ready: %v, %v, image %q, want it placed in %q", claimed, err, resolved.Image, image)
	}
	if stored := w.get(t, id); stored.ResolvedJob == nil || stored.ResolvedJob.Image != image || stored.Job.Image != "registry.test/job:1" {
		t.Errorf("stored image %+v and job image %q, want the environment's resolved and the named one kept", stored.ResolvedJob, stored.Job.Image)
	}
}

// A build goes to a live cluster that builds for the environment's org: its own or a shared one,
// not one it turned off and not another org's, the least loaded first and at most two at once.
func TestABuildIsAssignedToTheLeastLoadedLiveClusterThatBuildsForItsOrg(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	if has, err := w.envs.OrgHasEnvironmentBuilder(ctx, w.orgA); err != nil || !has {
		t.Fatalf("org a has a builder: %v, %v", has, err)
	}

	first := w.ensure(t, w.orgA, "first")
	if got := w.assign(t, first.Key, w.ofB, w.unregisteredName); got.ClusterName != "" || got.Attempt != 0 || got.State != domain.EnvironmentBuilding {
		t.Fatalf("with only another org's cluster live: %+v, want it assigned to none and no attempt spent", got)
	}
	if got := w.assign(t, first.Key); got.ClusterName != "" {
		t.Fatalf("with no cluster live: assigned to %q", got.ClusterName)
	}
	if got := w.assign(t, first.Key, w.ofA, w.ofB); got.ClusterName != w.ofA || got.Attempt != 1 || got.AssignedAt == nil {
		t.Fatalf("first: %+v, want it on org a's own cluster, attempt 1", got)
	}
	if again := w.assign(t, first.Key, w.ofA, w.ofB); again.Attempt != 1 || again.ClusterName != w.ofA {
		t.Fatalf("a live assignment within its lease was assigned again: %+v", again)
	}

	if _, _, err := w.clusters.UpdateCluster(ctx, w.ofB, sharedAs(true), domain.ClusterSettings{}); err != nil {
		t.Fatal(err)
	}
	second := w.ensure(t, w.orgA, "second")
	if got := w.assign(t, second.Key, w.ofA, w.ofB); got.ClusterName != w.ofB {
		t.Fatalf("second: on %q, want the shared cluster, which builds nothing yet", got.ClusterName)
	}
	third := w.ensure(t, w.orgA, "third")
	fourth := w.ensure(t, w.orgA, "fourth")
	fifth := w.ensure(t, w.orgA, "fifth")
	w.assign(t, third.Key, w.ofA, w.ofB)
	w.assign(t, fourth.Key, w.ofA, w.ofB)
	if got := w.assign(t, fifth.Key, w.ofA, w.ofB); got.ClusterName != "" || got.Attempt != 0 {
		t.Fatalf("fifth with both clusters building two: %+v, want it waiting", got)
	}

	if _, err := w.clusters.SetClusterEnabled(ctx, w.ofA, w.orgA, "test", false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.clusters.SetClusterEnabled(ctx, w.ofB, w.orgA, "test", false); err != nil {
		t.Fatal(err)
	}
	if has, err := w.envs.OrgHasEnvironmentBuilder(ctx, w.orgA); err != nil || has {
		t.Fatalf("org a turned every cluster off and has a builder: %v, %v", has, err)
	}
	if _, err := w.clusters.SetClusterEnabled(ctx, w.ofB, w.orgA, "test", true); err != nil {
		t.Fatal(err)
	}
	if err := w.clusters.RecordBuilds(ctx, w.ofB, false); err != nil {
		t.Fatal(err)
	}
	if has, err := w.envs.OrgHasEnvironmentBuilder(ctx, w.orgA); err != nil || has {
		t.Fatalf("org a's only enabled cluster stopped building and it has a builder: %v, %v", has, err)
	}
	if has, err := w.envs.OrgHasEnvironmentBuilder(ctx, w.orgB); err != nil || has {
		t.Fatalf("org b's cluster stopped building and it has a builder: %v, %v", has, err)
	}
}

// A build whose cluster stopped reporting or whose lease passed goes to the next cluster as a new
// attempt, the report of the old one is refused, and the fourth assignment fails it as the
// platform's fault.
func TestABuildIsReassignedOffADeadClusterOrAPassedLeaseUntilItsAttemptsAreSpent(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	if _, _, err := w.clusters.UpdateCluster(ctx, w.ofB, sharedAs(true), domain.ClusterSettings{}); err != nil {
		t.Fatal(err)
	}
	env := w.ensure(t, w.orgA, "reassigned")
	first := w.assign(t, env.Key, w.ofA, w.ofB)
	if first.ClusterName != w.ofA || first.Attempt != 1 {
		t.Fatalf("first: %+v", first)
	}
	if gone := w.assign(t, env.Key); gone.ClusterName != "" || gone.Attempt != 1 || gone.State != domain.EnvironmentBuilding {
		t.Fatalf("with no cluster live: %+v, want it taken from %s and waiting at attempt 1", gone, w.ofA)
	}

	second := w.assign(t, env.Key, w.ofB)
	if second.ClusterName != w.ofB || second.Attempt != 2 {
		t.Fatalf("after %s stopped reporting: %+v, want attempt 2 on %s", w.ofA, second, w.ofB)
	}
	if w.finish(t, w.ofA, first, domain.EnvironmentBuildOutcome{Image: "registry.test/env@sha256:stale"}) {
		t.Fatal("the cluster the build was taken from reported it")
	}
	if got, err := w.envs.AssignedEnvironmentBuild(ctx, w.ofA, env.Key, 1); err != nil || got != nil {
		t.Fatalf("the old attempt: %+v, %v, want it no longer the old cluster's", got, err)
	}
	if got, _ := w.envs.AssignedEnvironmentBuild(ctx, w.ofB, env.Key, 2); got == nil {
		t.Fatal("the new attempt is not the new cluster's")
	}
	if listed, err := w.envs.AssignedEnvironmentBuilds(ctx, w.ofB); err != nil || len(listed) != 1 || listed[0].Key != env.Key || listed[0].Attempt != 2 {
		t.Fatalf("builds of %s: %v, %v", w.ofB, listed, err)
	}

	if _, err := w.pool.pool.Exec(ctx, `UPDATE environments SET assigned_at = now() - interval '31 minutes' WHERE key = $1`, env.Key); err != nil {
		t.Fatal(err)
	}
	third := w.assign(t, env.Key, w.ofA, w.ofB)
	if third.Attempt != 3 || third.ClusterName != w.ofA {
		t.Fatalf("after the lease passed: %+v, want attempt 3 on the least loaded", third)
	}

	if !w.finish(t, w.ofA, third, domain.EnvironmentBuildOutcome{Fault: domain.EnvironmentFaultPlatform, LogTail: "pull timed out"}) {
		t.Fatal("a platform fault of the assigned cluster was refused")
	}
	if freed, _ := w.envs.GetEnvironment(ctx, env.Key); freed.State != domain.EnvironmentBuilding || freed.ClusterName != "" || freed.Attempt != 3 {
		t.Fatalf("after the platform's fault: %+v, want it freed for its next assignment", freed)
	}
	failed := w.assign(t, env.Key, w.ofA, w.ofB)
	if failed.State != domain.EnvironmentFailed || failed.Fault != domain.EnvironmentFaultPlatform || failed.ClusterName != "" || failed.Attempt != domain.MaxEnvironmentAttempts {
		t.Fatalf("the fourth assignment: %+v, want it failed as the platform's fault", failed)
	}

	again, err := w.envs.EnsureEnvironment(ctx, env)
	if err != nil || again.State != domain.EnvironmentBuilding || again.Fault != "" || again.Attempt != 0 || again.ClusterName != "" || again.LogTail != "" {
		t.Fatalf("ensured again: %+v, %v, want it building from its first attempt", again, err)
	}
}

// Only the cluster a build is assigned to, at its attempt, reports it, once. A file's fault fails
// it, and the next submit builds it again; a ready environment stays ready.
func TestOnlyTheAssignedAttemptReportsABuildAndAFailedOneIsBuiltAgain(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	env := w.assign(t, w.ensure(t, w.orgA, "fenced").Key, w.ofA)
	for name, c := range map[string]struct {
		cluster string
		attempt int
	}{"another cluster": {w.ofB, env.Attempt}, "another attempt": {w.ofA, env.Attempt + 1}} {
		if took, err := w.envs.FinishEnvironmentBuild(ctx, c.cluster, env.Key, c.attempt, domain.EnvironmentBuildOutcome{Image: "registry.test/env@sha256:x"}); err != nil || took {
			t.Fatalf("%s: took %v, %v", name, took, err)
		}
	}
	if !w.finish(t, w.ofA, env, domain.EnvironmentBuildOutcome{Fault: domain.EnvironmentFaultFile, LogTail: "E: Unable to locate package"}) {
		t.Fatal("the file's fault was refused")
	}
	if w.finish(t, w.ofA, env, domain.EnvironmentBuildOutcome{Image: "registry.test/env@sha256:late"}) {
		t.Fatal("a finished build was reported twice")
	}
	failed, _ := w.envs.GetEnvironment(ctx, env.Key)
	if failed.State != domain.EnvironmentFailed || failed.Fault != domain.EnvironmentFaultFile || failed.LogTail != "E: Unable to locate package" || failed.Image != "" {
		t.Fatalf("after the file's fault: %+v", failed)
	}
	if assigned := w.assign(t, env.Key, w.ofA); assigned.State != domain.EnvironmentFailed {
		t.Fatalf("a failed environment was assigned: %+v", assigned)
	}
	again, err := w.envs.EnsureEnvironment(ctx, env)
	if err != nil || again.State != domain.EnvironmentBuilding || again.Attempt != 0 || again.Fault != "" {
		t.Fatalf("ensured again: %+v, %v", again, err)
	}

	ready := w.assign(t, env.Key, w.ofA)
	if !w.finish(t, w.ofA, ready, domain.EnvironmentBuildOutcome{Image: "registry.test/env@sha256:ok"}) {
		t.Fatal("the build was refused")
	}
	if kept, err := w.envs.EnsureEnvironment(ctx, env); err != nil || kept.State != domain.EnvironmentReady || kept.Image != "registry.test/env@sha256:ok" {
		t.Fatalf("a ready environment ensured: %+v, %v, want it kept", kept, err)
	}
}

// A job ended because the platform did not build its environment ends there, refunded by its
// settlement, and is not queued again while the environment stays failed.
func TestAJobEndedForAnUnbuiltEnvironmentIsNotRequeued(t *testing.T) {
	w := newEnvironments(t)
	store := NewStore(w.pool, "http://metrics.invalid", 3)
	id := w.queued(t, "unbuilt", w.peA)
	reason := string(domain.EvictionEnvironmentUnbuilt.WithDetail("no fault of the file's"))
	got, err := store.ResolveTermination(context.Background(), id, domain.StatusQueued, domain.StatusEvicted, reason)
	if err != nil || got != domain.TerminationWritten {
		t.Fatalf("ResolveTermination: %v, %v, want it written", got, err)
	}
	if ended := w.get(t, id); ended.Status != domain.StatusEvicted || ended.EvictionReason != reason || ended.InfraRequeueCount != 0 {
		t.Fatalf("ended: %s, %q, %d infra requeues, want it evicted and not queued again", ended.Status, ended.EvictionReason, ended.InfraRequeueCount)
	}
}

// A database from when one builder built every org's files keyed without the org: a row a job
// named takes that job's org, an abandoned one fails as the platform's fault, and one no job named
// goes.
func TestTheSchemaGivesAnEnvironmentTheOrgOfItsJobAndFailsAnAbandonedOne(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	named, unnamed := "old-named-"+w.suffix, "old-unnamed-"+w.suffix
	job := w.queued(t, "old-env-job", w.peB)
	for _, statement := range []string{
		`ALTER TABLE environments DROP CONSTRAINT environments_state_check`,
		`ALTER TABLE environments ALTER COLUMN org_id DROP NOT NULL`,
		`ALTER TABLE environments ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0`,
		`INSERT INTO environments (key, base, dockerfile, image, state, attempts) VALUES
			('` + named + `', 'b', 'f', 'registry.test/env:` + named + `', 'abandoned', 3),
			('` + unnamed + `', 'b', 'f', 'registry.test/env:` + unnamed + `', 'ready', 0)`,
		`UPDATE experiments SET environment_key = '` + named + `' WHERE id = '` + job + `'`,
	} {
		if _, err := w.pool.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := applySchemaAt(ctx, testDSN()); err != nil {
			t.Fatal(err)
		}
	}
	got, err := w.envs.GetEnvironment(ctx, named)
	if err != nil || got == nil || got.OrgID != w.orgB || got.State != domain.EnvironmentFailed || got.Fault != domain.EnvironmentFaultPlatform {
		t.Fatalf("the named row: %+v, %v, want org b's, failed as the platform's fault", got, err)
	}
	if gone, err := w.envs.GetEnvironment(ctx, unnamed); err != nil || gone != nil {
		t.Fatalf("the unnamed row: %+v, %v, want it gone", gone, err)
	}
}

func TestAssigningAnEnvironmentNoRowNamesIsNothing(t *testing.T) {
	w := newEnvironments(t)
	if env, err := w.envs.AssignEnvironment(context.Background(), "no-such-"+w.suffix, []string{w.ofA}, time.Minute); err != nil || env != nil {
		t.Fatalf("got %+v, %v", env, err)
	}
}

// A build's progress is the assigned cluster's to report, at its attempt, while it builds, and an
// assignment of the next attempt starts it over.
func TestABuildsProgressIsTheAssignedAttemptsAndANewAssignmentClearsIt(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	env := w.assign(t, w.ensure(t, w.orgA, "progress").Key, w.ofA)
	for name, c := range map[string]struct {
		cluster string
		attempt int
	}{"another cluster": {w.ofB, env.Attempt}, "another attempt": {w.ofA, env.Attempt + 1}} {
		if took, err := w.envs.RecordEnvironmentProgress(ctx, c.cluster, env.Key, c.attempt, "[1/3] FROM base: started"); err != nil || took {
			t.Fatalf("%s: took %v, %v", name, took, err)
		}
	}
	if took, err := w.envs.RecordEnvironmentProgress(ctx, w.ofA, env.Key, env.Attempt, "[2/3] RUN apt-get install gcc: started"); err != nil || !took {
		t.Fatalf("the assigned attempt: took %v, %v", took, err)
	}
	got, _ := w.envs.GetEnvironment(ctx, env.Key)
	if got.Progress != "[2/3] RUN apt-get install gcc: started" || got.ProgressAt == nil {
		t.Fatalf("progress = %q at %v", got.Progress, got.ProgressAt)
	}
	if again := w.assign(t, env.Key); again.Progress != "" || again.ProgressAt != nil {
		t.Fatalf("taken from its cluster: progress %q at %v, want none", again.Progress, again.ProgressAt)
	}
}
