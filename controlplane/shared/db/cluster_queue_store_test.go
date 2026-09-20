package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// The checkpoint-window grant is derived from this query, and a preemption clears cluster_name in
// the same UPDATE that records the termination (RequeuePreempted: the job holds no capacity any
// more, so the next admission tick may place it anywhere). Narrowing this read by cluster_name
// therefore asked a row that had just forgotten its cluster which cluster it was on, matched
// nothing, and left the grant list permanently empty — so every preempted job was killed on the
// ordinary 5s shutdown grace and lost the checkpoint the requeue's rescaled estimate assumes it
// wrote. It narrows by last_cluster, which no termination clears.
func TestRecentlyUndesiredWorkloadsIsNotNarrowedByAClusterTheTerminationJustCleared(t *testing.T) {
	if strings.Contains(recentlyUndesiredWorkloadsQuery, " cluster_name =") {
		t.Fatalf("query narrows by cluster_name, which a termination clears in the same write: %s", recentlyUndesiredWorkloadsQuery)
	}
	if !strings.Contains(recentlyUndesiredWorkloadsQuery, "last_cluster = $1") {
		t.Fatalf("query is not narrowed to the cluster the job ran on, so a cluster is told of every org's terminations: %s", recentlyUndesiredWorkloadsQuery)
	}
}

// The answer has to expire on its own, which is the entire reason there is no terminating state
// to write, advance or clear. Without the time bound every experiment ever finished reads as
// "recently undesired" and a grant would outlive the window it grants.
func TestRecentlyUndesiredWorkloadsIsBoundedByStatusAndTime(t *testing.T) {
	if !strings.Contains(recentlyUndesiredWorkloadsQuery, "status <> ALL($2)") {
		t.Fatalf("query does not exclude the desired statuses: %s", recentlyUndesiredWorkloadsQuery)
	}
	if !strings.Contains(recentlyUndesiredWorkloadsQuery, "eviction_reason IS NOT NULL") {
		t.Fatalf("query returns rows no termination was written to: %s", recentlyUndesiredWorkloadsQuery)
	}
	if !strings.Contains(recentlyUndesiredWorkloadsQuery, "updated_at > now() - $3::interval") {
		t.Fatalf("query is not time-bounded, so a grant would never expire: %s", recentlyUndesiredWorkloadsQuery)
	}
}

// The scheduler rewrites a queued job's priority and queue reason every tick. Neither may move
// updated_at, or a preempted job stays "recently undesired" for as long as it waits and its
// checkpoint grant never expires.
func TestAPreemptedJobStopsBeingRecentlyUndesiredWhileItWaitsInTheQueue(t *testing.T) {
	pool, _, agentID, peID := setupCapTestPool(t, 4)
	exps, queue := NewExperimentsStore(pool), NewClusterQueueStore(pool)
	ctx := context.Background()
	preempted, fresh := "undesired-preempted-"+peID, "undesired-fresh-"+peID
	const cluster = "cluster-undesired"
	if _, err := NewClustersStore(pool).RegisterCluster(ctx, cluster, "default"); err != nil {
		t.Fatal(err)
	}
	seedExperimentForCapTest(t, pool, preempted, agentID, peID, 1, domain.StatusQueued)
	seedExperimentForCapTest(t, pool, fresh, agentID, peID, 1, domain.StatusQueued)
	if ok, err := exps.ClaimSubmitted(ctx, preempted, cluster, &domain.JobSpec{AcceleratorCount: 1}, nil); err != nil || !ok {
		t.Fatalf("ClaimSubmitted = %v, %v, want true", ok, err)
	}
	if ok, err := exps.MarkStarted(ctx, preempted); err != nil || !ok {
		t.Fatalf("MarkStarted = %v, %v, want true", ok, err)
	}
	if ok, err := exps.RequeuePreempted(ctx, preempted, 1, 1); err != nil || !ok {
		t.Fatalf("RequeuePreempted = %v, %v, want true", ok, err)
	}
	listed := func() map[string]bool {
		t.Helper()
		undesired, err := queue.ListRecentlyUndesiredWorkloads(ctx, cluster, time.Minute)
		if err != nil {
			t.Fatalf("ListRecentlyUndesiredWorkloads: %v", err)
		}
		ids := map[string]bool{}
		for _, exp := range undesired {
			ids[exp.ID] = true
		}
		return ids
	}
	if ids := listed(); !ids[preempted] || ids[fresh] {
		t.Fatalf("listed = %v, want the preempted job and not the never-run one", ids)
	}

	if _, err := pool.pool.Exec(ctx, `UPDATE experiments SET updated_at = now() - interval '2 minutes' WHERE id = $1`, preempted); err != nil {
		t.Fatalf("age the requeue: %v", err)
	}
	if err := exps.UpdateExperimentPriority(ctx, preempted, 0.75); err != nil {
		t.Fatalf("UpdateExperimentPriority: %v", err)
	}
	if err := exps.UpdateNotAdmittedReason(ctx, preempted, domain.NotAdmittedCapacityUnavailable); err != nil {
		t.Fatalf("UpdateNotAdmittedReason: %v", err)
	}
	if listed()[preempted] {
		t.Fatal("a tick's priority and reason writes made an old requeue recent again")
	}
	got, err := exps.GetExperiment(ctx, preempted)
	if err != nil || got.PriorityScore != 0.75 || got.NotAdmittedReason != domain.NotAdmittedCapacityUnavailable {
		t.Fatalf("after the writes: priority %v, reason %q, err %v", got.PriorityScore, got.NotAdmittedReason, err)
	}
}
