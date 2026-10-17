package db

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
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

// projectOf is a project of org, which an environment is built for.
func (w *environments) projectOf(t *testing.T, org string) string {
	t.Helper()
	var id string
	if err := w.pool.pool.QueryRow(context.Background(), `SELECT id FROM projects WHERE org_id = $1 ORDER BY id LIMIT 1`, org).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (w *environments) ensure(t *testing.T, org, what string) *domain.Environment {
	t.Helper()
	dockerfile := "ARG BASE\nFROM ${BASE}\nRUN echo " + w.suffix + " " + what + "\n"
	env, err := w.envs.EnsureEnvironment(context.Background(), &domain.Environment{Key: domain.EnvironmentKey(org, "", dockerfile, "registry.test/job:1", ""), OrgID: org, ProjectID: w.projectOf(t, org), GitRepository: "owner/repo", Base: "registry.test/job:1", Dockerfile: dockerfile})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (w *environments) assign(t *testing.T, key string, live ...string) *domain.Environment {
	t.Helper()
	return w.assignHeld(t, key, live, live)
}

// assignHeld assigns the build of key with live the clusters it may go to and holding those that
// keep the builds they hold.
func (w *environments) assignHeld(t *testing.T, key string, live, holding []string) *domain.Environment {
	t.Helper()
	env, err := w.envs.AssignEnvironment(context.Background(), key, live, holding, domain.EnvironmentBuildLease)
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

	if _, err := w.clusters.SetClusterEnabled(ctx, w.ofA, w.orgA, domain.Actor{Kind: domain.ActorUser, ID: "test"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.clusters.SetClusterEnabled(ctx, w.ofB, w.orgA, domain.Actor{Kind: domain.ActorUser, ID: "test"}, false); err != nil {
		t.Fatal(err)
	}
	if has, err := w.envs.OrgHasEnvironmentBuilder(ctx, w.orgA); err != nil || has {
		t.Fatalf("org a turned every cluster off and has a builder: %v, %v", has, err)
	}
	if _, err := w.clusters.SetClusterEnabled(ctx, w.ofB, w.orgA, domain.Actor{Kind: domain.ActorUser, ID: "test"}, true); err != nil {
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

// A build whose cluster stopped reporting for longer than the hold or whose lease passed goes to
// the next cluster as a new attempt, the report of the old one is refused, and the fourth
// assignment fails it as the platform's fault.
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
	if held := w.assignHeld(t, env.Key, []string{w.ofB}, []string{w.ofA, w.ofB}); held.ClusterName != w.ofA || held.Attempt != 1 {
		t.Fatalf("with %s silent but heard from within the hold: %+v, want it kept at attempt 1", w.ofA, held)
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

// A build was taken from a cluster that went silent and assigned again elsewhere. The
// file is the key's, so the old attempt's report of the file failing fails the build at once and
// the new attempt does not run it again; a later attempt's report is no earlier attempt's.
func TestAFileFaultOfAnEarlierAttemptFailsTheBuildWhereverItIsNow(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	if _, _, err := w.clusters.UpdateCluster(ctx, w.ofB, sharedAs(true), domain.ClusterSettings{}); err != nil {
		t.Fatal(err)
	}
	env := w.ensure(t, w.orgA, "moved")
	first := w.assign(t, env.Key, w.ofA)
	w.assign(t, env.Key)
	second := w.assign(t, env.Key, w.ofB)
	if second.ClusterName != w.ofB || second.Attempt != 2 {
		t.Fatalf("second: %+v", second)
	}
	if took, err := w.envs.FinishEnvironmentBuild(ctx, w.ofB, env.Key, 3, domain.EnvironmentBuildOutcome{Fault: domain.EnvironmentFaultFile, LogTail: "x"}); err != nil || took {
		t.Fatalf("a report of an attempt not yet made: took %v, %v", took, err)
	}
	if !w.finish(t, w.ofA, first, domain.EnvironmentBuildOutcome{Fault: domain.EnvironmentFaultFile, LogTail: "error: unexpected argument '--locked=false'"}) {
		t.Fatal("the file's fault reported by the earlier attempt was refused")
	}
	failed, _ := w.envs.GetEnvironment(ctx, env.Key)
	if failed.State != domain.EnvironmentFailed || failed.Fault != domain.EnvironmentFaultFile || failed.Attempt != 2 {
		t.Fatalf("after the earlier attempt's file fault: %+v, want it failed as the file's", failed)
	}
	if listed, err := w.envs.AssignedEnvironmentBuilds(ctx, w.ofB); err != nil || len(listed) != 0 {
		t.Fatalf("builds of %s: %v, %v, want none left to run", w.ofB, listed, err)
	}
}

// Only the cluster a build is assigned to, at its attempt, reports it, once. A file's fault fails
// it, and the next submit builds it again with that build's output kept until it is built; a ready
// environment stays ready.
func TestOnlyTheAssignedAttemptReportsABuildAndAFailedOneIsBuiltAgainWithItsLastOutput(t *testing.T) {
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
	if err != nil || again.State != domain.EnvironmentBuilding || again.Attempt != 0 || again.Fault != "" || again.LogTail != "" || again.FailedLog != "E: Unable to locate package" {
		t.Fatalf("ensured again: %+v, %v, want it building from its first attempt with the file's last output", again, err)
	}

	ready := w.assign(t, env.Key, w.ofA)
	if !w.finish(t, w.ofA, ready, domain.EnvironmentBuildOutcome{Image: "registry.test/env@sha256:ok"}) {
		t.Fatal("the build was refused")
	}
	if kept, err := w.envs.EnsureEnvironment(ctx, env); err != nil || kept.State != domain.EnvironmentReady || kept.Image != "registry.test/env@sha256:ok" || kept.FailedLog != "" {
		t.Fatalf("a ready environment ensured: %+v, %v, want it kept", kept, err)
	}
}

// A job ended because the platform did not build its environment ends there, its hold released
// by the same write that tells its agent, and is not queued again while the environment stays
// failed. A job no longer queued is left as it is.
func TestAJobEndedForAnUnbuiltEnvironmentIsSettledWithItsStatusAndNotRequeued(t *testing.T) {
	w := newEnvironments(t)
	store := NewStore(w.pool, 3)
	id := w.queued(t, "unbuilt", w.peA)
	reason := string(domain.EvictionEnvironmentUnbuilt.WithDetail("no fault of the file's"))
	written, err := store.EndQueuedSettled(context.Background(), id, domain.StatusEvicted, reason)
	if err != nil || !written {
		t.Fatalf("EndQueuedSettled: %v, %v, want it written", written, err)
	}
	ended := w.get(t, id)
	if ended.Status != domain.StatusEvicted || ended.EvictionReason != reason || ended.InfraRequeueCount != 0 || ended.QuotaSettledAt == nil {
		t.Fatalf("ended: %s, %q, %d infra requeues, settled %v, want it evicted, settled and not queued again", ended.Status, ended.EvictionReason, ended.InfraRequeueCount, ended.QuotaSettledAt)
	}
	if again, err := store.EndQueuedSettled(context.Background(), id, domain.StatusRejected, "other"); err != nil || again {
		t.Fatalf("an ended job ended again: %v, %v", again, err)
	}
}

func TestAssigningAnEnvironmentNoRowNamesIsNothing(t *testing.T) {
	w := newEnvironments(t)
	if env, err := w.envs.AssignEnvironment(context.Background(), "no-such-"+w.suffix, []string{w.ofA}, []string{w.ofA}, time.Minute); err != nil || env != nil {
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

// A build its cluster holds on one node places a job before it is ready, but only in the image
// held there, which the admission pinned the job to, on that cluster, at that attempt. Any new
// assignment and any outcome forgets it.
func TestABuildLoadedOnANodePlacesAJobOnlyInThatImageOnItsCluster(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	env := w.assign(t, w.ensure(t, w.orgA, "loaded").Key, w.ofA)
	id := w.queued(t, "loaded-job", w.peA)
	if _, err := w.pool.pool.Exec(ctx, `UPDATE experiments SET environment_key = $2 WHERE id = $1`, id, env.Key); err != nil {
		t.Fatal(err)
	}
	image := "registry.test/env@sha256:" + w.suffix
	for name, c := range map[string]struct {
		cluster string
		attempt int
	}{"another cluster": {w.ofB, env.Attempt}, "another attempt": {w.ofA, env.Attempt + 1}} {
		if took, err := w.envs.RecordEnvironmentLoaded(ctx, c.cluster, env.Key, c.attempt, "node-1", image); err != nil || took {
			t.Fatalf("%s: took %v, %v", name, took, err)
		}
	}
	if took, err := w.envs.RecordEnvironmentLoaded(ctx, w.ofA, env.Key, env.Attempt, "node-1", image); err != nil || !took {
		t.Fatalf("the assigned attempt: took %v, %v", took, err)
	}
	if got, _ := w.envs.GetEnvironment(ctx, env.Key); got.State != domain.EnvironmentBuilding || got.LoadedImage != image || got.LoadedNode != "node-1" || got.LoadedCluster != w.ofA || got.Image != "" {
		t.Fatalf("loaded: %+v, want it building, held on node-1 in %s", got, image)
	}

	if w.claim(t, id, w.ofA) {
		t.Fatal("a job not pinned to the loaded image was placed before the push")
	}
	pinned := &domain.JobSpec{Image: image, AcceleratorCount: 1, NodeSelector: map[string]string{domain.NodeHostnameLabel: "node-1"}}
	if claimed, err := w.exps.ClaimSubmitted(ctx, id, w.ofA, pinned, nil); err != nil || !claimed || pinned.Image != image {
		t.Fatalf("claim pinned to the loaded image: %v, %v, image %q", claimed, err, pinned.Image)
	}
	if stored := w.get(t, id); stored.ResolvedJob == nil || stored.ResolvedJob.Image != image || stored.ResolvedJob.NodeSelector[domain.NodeHostnameLabel] != "node-1" {
		t.Errorf("stored %+v, want the loaded image pinned to its node", stored.ResolvedJob)
	}
}

// A push that failed after the build was loaded into a node leaves that node running the build's
// jobs while it is built again, on whatever cluster, and a later attempt that loads it elsewhere
// names that node instead. Once the build ends nothing is held.
func TestALoadedBuildWhosePushFailedStillRunsItsJobsOnItsNode(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	image := "registry.test/env@sha256:" + w.suffix
	env := w.assign(t, w.ensure(t, w.orgA, "push failed").Key, w.ofA)
	if took, err := w.envs.RecordEnvironmentLoaded(ctx, w.ofA, env.Key, env.Attempt, "node-1", image); err != nil || !took {
		t.Fatalf("loaded: took %v, %v", took, err)
	}
	if !w.finish(t, w.ofA, env, domain.EnvironmentBuildOutcome{Fault: domain.EnvironmentFaultPlatform, LogTail: "push: 503"}) {
		t.Fatal("the platform's fault was refused")
	}
	if got, _ := w.envs.GetEnvironment(ctx, env.Key); got.State != domain.EnvironmentBuilding || got.ClusterName != "" || got.LoadedImage != image || got.LoadedCluster != w.ofA || got.LoadedNode != "node-1" {
		t.Fatalf("after the failed push: %+v, want it building on none and still held on node-1 of %s", got, w.ofA)
	}
	if _, err := w.pool.pool.Exec(ctx, `UPDATE clusters SET shared = true, priority = 0 WHERE name = $1`, w.ofB); err != nil {
		t.Fatal(err)
	}
	next := w.assign(t, env.Key, w.ofB)
	if next.ClusterName != w.ofB || next.Attempt != env.Attempt+1 || next.LoadedImage != image || next.LoadedCluster != w.ofA {
		t.Fatalf("built again: %+v, want it on %s, still held on %s", next, w.ofB, w.ofA)
	}
	id := w.queued(t, "push-failed-job", w.peA)
	if _, err := w.pool.pool.Exec(ctx, `UPDATE experiments SET environment_key = $2 WHERE id = $1`, id, env.Key); err != nil {
		t.Fatal(err)
	}
	if w.claim(t, id, w.ofB) {
		t.Fatal("a job was placed on the cluster that builds it again before it is pushed or loaded there")
	}
	pinned := &domain.JobSpec{Image: image, AcceleratorCount: 1, NodeSelector: map[string]string{domain.NodeHostnameLabel: "node-1"}}
	if claimed, err := w.exps.ClaimSubmitted(ctx, id, w.ofA, pinned, nil); err != nil || !claimed {
		t.Fatalf("claim on the node that holds it: %v, %v", claimed, err)
	}

	elsewhere := image + "0"
	if took, err := w.envs.RecordEnvironmentLoaded(ctx, w.ofB, env.Key, next.Attempt, "node-2", elsewhere); err != nil || !took {
		t.Fatalf("loaded again: took %v, %v", took, err)
	}
	if got, _ := w.envs.GetEnvironment(ctx, env.Key); got.LoadedImage != elsewhere || got.LoadedCluster != w.ofB || got.LoadedNode != "node-2" {
		t.Fatalf("loaded again: %+v, want node-2 of %s", got, w.ofB)
	}
	if !w.finish(t, w.ofB, next, domain.EnvironmentBuildOutcome{Image: elsewhere}) {
		t.Fatal("the push was refused")
	}
	if got, _ := w.envs.GetEnvironment(ctx, env.Key); got.State != domain.EnvironmentReady || got.LoadedImage != "" || got.LoadedCluster != "" || got.LoadedNode != "" {
		t.Errorf("ready: %+v, want nothing held", got)
	}

	spent := w.assign(t, w.ensure(t, w.orgA, "push failed every time").Key, w.ofA)
	for attempt := spent.Attempt; attempt <= domain.MaxEnvironmentAttempts; attempt++ {
		if took, err := w.envs.RecordEnvironmentLoaded(ctx, w.ofA, spent.Key, attempt, "node-1", image); err != nil || !took {
			t.Fatalf("attempt %d loaded: took %v, %v", attempt, took, err)
		}
		if !w.finish(t, w.ofA, &domain.Environment{Key: spent.Key, Attempt: attempt}, domain.EnvironmentBuildOutcome{Fault: domain.EnvironmentFaultPlatform}) {
			t.Fatalf("attempt %d: the platform's fault was refused", attempt)
		}
		spent = w.assign(t, spent.Key, w.ofA)
	}
	if spent.State != domain.EnvironmentFailed || spent.LoadedImage != "" || spent.LoadedCluster != "" {
		t.Errorf("every attempt spent: %+v, want it failed holding nothing", spent)
	}
}

func TestAnEnvironmentKeepsTheImageItsToolsComeFrom(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	dockerfile := "ARG BASE\nFROM ${BASE}\nRUN echo " + w.suffix + " tools\n"
	base, tools := "registry.test/job-base@sha256:"+w.suffix, "registry.test/job@sha256:"+w.suffix
	made, err := w.envs.EnsureEnvironment(ctx, &domain.Environment{Key: domain.EnvironmentKey(w.orgA, "", dockerfile, base, tools), OrgID: w.orgA, ProjectID: w.projectOf(t, w.orgA), GitRepository: "owner/repo", Base: base, Tools: tools, Dockerfile: dockerfile})
	if err != nil {
		t.Fatal(err)
	}
	assigned := w.assign(t, made.Key, w.ofA)
	builds, err := w.envs.AssignedEnvironmentBuilds(ctx, w.ofA)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range builds {
		if b.Key == assigned.Key && (b.Base != base || b.Tools != tools) {
			t.Errorf("assigned build on %q with tools of %q, want %q and %q", b.Base, b.Tools, base, tools)
		}
	}
	if made.Tools != tools || assigned.Tools != tools {
		t.Errorf("made %+v and assigned %+v, want both with the tools' image", made, assigned)
	}
}

// An agent that asks for a build while it builds is told once it ends, live and replayed, with
// the outcome and, once the file failed, where the end of its output is read. One that asks of a
// build already over waits for nothing, and asking again once it ended waits for the next build.
func TestAnAgentThatAskedForABuildIsToldItEnded(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	codeRef := "https://example.com/repo.git@" + strings.Repeat("c", 40)
	before := cursorNow(t, w.pool)
	events, stop := listenForEvents(t, w.pool)
	defer stop()

	failing := w.ensure(t, w.orgA, "told failing")
	for _, image := range []string{"pytorch", "another"} {
		if err := w.envs.AwaitEnvironmentBuild(ctx, failing.Key, w.peA, w.agent, codeRef, image); err != nil {
			t.Fatal(err)
		}
	}
	env := w.assign(t, failing.Key, w.ofA)
	if !w.finish(t, w.ofA, env, domain.EnvironmentBuildOutcome{Fault: domain.EnvironmentFaultFile, LogTail: "E: no package"}) {
		t.Fatal("the assigned cluster's report was refused")
	}
	failed := awaitEvent(t, events, domain.EventEnvironmentStatus, failing.Key)
	if failed.Value != "failed" || failed.AgentID != w.agent || failed.PlatformExperimentID != w.peA ||
		!strings.Contains(failed.Detail, "commit cccccccccccc ") || !strings.Contains(failed.Detail, "on pytorch") || !strings.Contains(failed.Detail, "`hl env status`") {
		t.Fatalf("failed: got = %+v, want it failed for the agent, naming the commit, the image and hl env status", failed)
	}
	if own := (domain.EventFilter{PlatformExperimentID: w.peA, AgentID: w.agent}); !own.Matches(failed) {
		t.Error("the agent's own subscription does not carry the end of its build")
	}
	if rival := (domain.EventFilter{PlatformExperimentID: w.peA, AgentID: "another"}); rival.Matches(failed) {
		t.Error("a rival's subscription carries the end of the agent's build")
	}

	ready := w.ensure(t, w.orgA, "told ready")
	if err := w.envs.AwaitEnvironmentBuild(ctx, ready.Key, w.peA, w.agent, codeRef, "pytorch"); err != nil {
		t.Fatal(err)
	}
	env = w.assign(t, ready.Key, w.ofA)
	if !w.finish(t, w.ofA, env, domain.EnvironmentBuildOutcome{Image: "registry.test/env@sha256:" + w.suffix}) {
		t.Fatal("the assigned cluster's report was refused")
	}
	built := awaitEvent(t, events, domain.EventEnvironmentStatus, ready.Key)
	if built.Value != "ready" || !strings.Contains(built.Detail, "is built on pytorch") {
		t.Fatalf("ready: got = %+v, want it built", built)
	}
	if decisions := (domain.EventFilter{PlatformExperimentID: w.peA, Decisions: true}); decisions.Matches(built) || !decisions.Matches(failed) {
		t.Error("a decisions stream carries a build that is ready, or not one that failed")
	}

	if err := w.envs.AwaitEnvironmentBuild(ctx, ready.Key, w.peA, w.agent, codeRef, "pytorch"); err != nil {
		t.Fatal(err)
	}
	replayed, err := NewEventsStore(w.pool, noHypothesisEvents{}).Replay(ctx, domain.EventFilter{PlatformExperimentID: w.peA, AgentID: w.agent}, before)
	if err != nil {
		t.Fatal(err)
	}
	var told []domain.Event
	for _, e := range replayed {
		if e.Kind == domain.EventEnvironmentStatus {
			told = append(told, e)
		}
	}
	if len(told) != 2 || told[0].Identity() != failed.Identity() || told[1].Identity() != built.Identity() {
		t.Fatalf("replayed: got = %+v, want the two live events %+v, %+v", told, failed, built)
	}

	again := w.ensure(t, w.orgA, "told failing")
	if err := w.envs.AwaitEnvironmentBuild(ctx, again.Key, w.peA, w.agent, codeRef, "pytorch"); err != nil {
		t.Fatal(err)
	}
	replayed, err = NewEventsStore(w.pool, noHypothesisEvents{}).Replay(ctx, domain.EventFilter{PlatformExperimentID: w.peA, AgentID: w.agent}, before)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range replayed {
		if e.Kind == domain.EventEnvironmentStatus && e.Subject == again.Key {
			t.Errorf("asked again once it failed: still told %+v, want it waiting for the next build", e)
		}
	}
}

// An agent waiting for a build is told once a cluster loaded it, naming that cluster, live and
// replayed. Its jobs run from then on, by themselves once the push ends, so the build ending ready
// tells it nothing more; a build that fails after still does.
func TestAnAgentIsToldItsBuildLoadedAndThenOnlyOfAFailure(t *testing.T) {
	w := newEnvironments(t)
	ctx := context.Background()
	codeRef := "https://example.com/repo.git@" + strings.Repeat("d", 40)
	image := "registry.test/env@sha256:" + w.suffix
	before := cursorNow(t, w.pool)
	events, stop := listenForEvents(t, w.pool)
	defer stop()

	pushed := w.ensure(t, w.orgA, "told loaded then pushed")
	failing := w.ensure(t, w.orgA, "told loaded then failed")
	var loaded []domain.Event
	for _, made := range []*domain.Environment{pushed, failing} {
		if err := w.envs.AwaitEnvironmentBuild(ctx, made.Key, w.peA, w.agent, codeRef, "pytorch"); err != nil {
			t.Fatal(err)
		}
		env := w.assign(t, made.Key, w.ofA)
		for range 2 {
			if took, err := w.envs.RecordEnvironmentLoaded(ctx, w.ofA, env.Key, env.Attempt, "node-1", image); err != nil || !took {
				t.Fatalf("loaded: took %v, %v", took, err)
			}
		}
		told := awaitEvent(t, events, domain.EventEnvironmentStatus, made.Key)
		if told.Value != "loaded" || told.AgentID != w.agent || !strings.Contains(told.Detail, "commit dddddddddddd ") ||
			!strings.Contains(told.Detail, "loaded on cluster "+w.ofA+" ") || !strings.Contains(told.Detail, "submit its jobs now") {
			t.Fatalf("loaded: got = %+v, want it loaded on %s, saying to submit", told, w.ofA)
		}
		loaded = append(loaded, told)
	}

	env, _ := w.envs.GetEnvironment(ctx, pushed.Key)
	if !w.finish(t, w.ofA, env, domain.EnvironmentBuildOutcome{Image: image}) {
		t.Fatal("the push was refused")
	}
	env, _ = w.envs.GetEnvironment(ctx, failing.Key)
	if !w.finish(t, w.ofA, env, domain.EnvironmentBuildOutcome{Fault: domain.EnvironmentFaultFile, LogTail: "E: no package"}) {
		t.Fatal("the file's fault was refused")
	}
	var failed domain.Event
	for failed.Kind == "" {
		select {
		case e := <-events:
			switch {
			case e.Kind == domain.EventEnvironmentStatus && e.Subject == pushed.Key:
				t.Fatalf("told %+v, want nothing more of a build told loaded that ended ready", e)
			case e.Kind == domain.EventEnvironmentStatus && e.Subject == failing.Key:
				failed = e
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the build that failed after it was loaded told nothing")
		}
	}
	if failed.Value != "failed" {
		t.Fatalf("failed: got = %+v", failed)
	}

	replayed, err := NewEventsStore(w.pool, noHypothesisEvents{}).Replay(ctx, domain.EventFilter{PlatformExperimentID: w.peA, AgentID: w.agent}, before)
	if err != nil {
		t.Fatal(err)
	}
	var told []domain.Event
	for _, e := range replayed {
		if e.Kind == domain.EventEnvironmentStatus {
			told = append(told, e.Identity())
		}
	}
	if want := []domain.Event{loaded[0].Identity(), loaded[1].Identity(), failed.Identity()}; !slices.Equal(told, want) {
		t.Errorf("replayed: got = %v, want the live events %v", told, want)
	}
}
