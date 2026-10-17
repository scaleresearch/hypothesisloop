package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

func (w *twoOrgs) bound(t *testing.T, cluster string) string {
	t.Helper()
	id := "rid-" + cluster
	if err := w.clusters.BindReportedID(context.Background(), cluster, id); err != nil {
		t.Fatalf("bind %s: %v", cluster, err)
	}
	return id
}

// Every infrastructure fault leaves the cluster it happened on in the job's tried list, under
// the identity the cluster's runtime reported, and spends one of the job's allowance. A scale-up
// that timed out is one of them: it used to be free, and a job could go round its clusters for
// ever.
func TestAnInfrastructureRequeueRecordsTheClusterAndSpendsTheAllowance(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	reported := w.bound(t, w.ofA)
	for name, reason := range map[string]string{
		"stuck": string(domain.EvictionStuckPending),
		"scale": string(domain.EvictionScaleUpTimeout),
		"told":  string(domain.EvictionScaleUpTimeout) + ": the autoscaler refused",
	} {
		t.Run(name, func(t *testing.T) {
			id := w.placed(t, "tried-"+name, w.peA, w.ofA, domain.StatusSubmitted)
			before := time.Now().Add(-time.Second)

			ok, err := w.exps.RequeueInfrastructureFault(ctx, id, domain.StatusSubmitted, reason, 2)
			if err != nil || !ok {
				t.Fatalf("requeue = %v, %v", ok, err)
			}
			got := w.get(t, id)
			if got.Status != domain.StatusQueued || got.ClusterName != "" || got.InfraRequeueCount != 1 || got.AttemptCount != 1 {
				t.Fatalf("after the requeue: %s on %q, infra %d, attempts %d, want it queued with one of each spent", got.Status, got.ClusterName, got.InfraRequeueCount, got.AttemptCount)
			}
			wantCode := string(domain.EvictionReason(reason).Code())
			if len(got.TriedClusters) != 1 || got.TriedClusters[0].ClusterID != reported || got.TriedClusters[0].Reason != wantCode ||
				got.TriedClusters[0].At.Before(before) || got.TriedClusters[0].At.After(time.Now().Add(time.Second)) {
				t.Fatalf("tried list = %+v, want one entry for %s with reason %s, written now", got.TriedClusters, reported, wantCode)
			}

			// Placed again and failed again: the second entry is appended and the allowance is gone.
			if !w.claim(t, id, w.ofA) {
				t.Fatal("the requeued job was not placed again")
			}
			if ok, err := w.exps.RequeueInfrastructureFault(ctx, id, domain.StatusSubmitted, reason, 2); err != nil || !ok {
				t.Fatalf("second requeue = %v, %v", ok, err)
			}
			if !w.claim(t, id, w.ofA) {
				t.Fatal("the requeued job was not placed a third time")
			}
			if ok, err := w.exps.RequeueInfrastructureFault(ctx, id, domain.StatusSubmitted, reason, 2); err != nil || ok {
				t.Fatalf("a requeue past the allowance = %v, %v, want it refused so the caller ends the job", ok, err)
			}
			last := w.get(t, id)
			if last.Status != domain.StatusSubmitted || last.InfraRequeueCount != 2 || len(last.TriedClusters) != 2 {
				t.Fatalf("after the refused requeue: %s, infra %d, tried %+v, want it left placed with two spent", last.Status, last.InfraRequeueCount, last.TriedClusters)
			}
		})
	}
}

// A job whose code could not be fetched, or whose offer was gone, failed for no reason of its
// cluster's: it is requeued on the same allowance and the cluster stays a candidate at once.
func TestARequeueThatIsNotTheClustersFaultLeavesTheTriedListAlone(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	w.bound(t, w.ofA)
	for name, reason := range map[string]string{
		"code":  string(domain.EvictionCodeUnreachable) + ": command exited with code 75: launch: the repository did not answer in 7 tries",
		"offer": string(domain.EvictionNoOffer) + ": no machine to rent in 3m0s of asking",
	} {
		id := w.placed(t, "untried-"+name, w.peA, w.ofA, domain.StatusSubmitted)
		if ok, err := w.exps.RequeueInfrastructureFault(ctx, id, domain.StatusSubmitted, reason, 2); err != nil || !ok {
			t.Fatalf("%s: requeue = %v, %v", name, ok, err)
		}
		got := w.get(t, id)
		if got.Status != domain.StatusQueued || got.InfraRequeueCount != 1 || got.AttemptCount != 1 || got.EvictionReason != reason || len(got.TriedClusters) != 0 {
			t.Fatalf("%s: %s, infra %d, attempts %d, reason %q, tried %+v, want it queued with the allowance spent and no cluster tried", name, got.Status, got.InfraRequeueCount, got.AttemptCount, got.EvictionReason, got.TriedClusters)
		}
	}
}

// A cluster whose runtime has reported no identity has nothing the tried list could name it by.
// The requeue still happens and still spends.
func TestAnInfrastructureRequeueOnAClusterWithNoReportedIdentityRecordsNothing(t *testing.T) {
	w := newTwoOrgs(t)
	id := w.placed(t, "tried-unbound", w.peA, w.ofA, domain.StatusSubmitted)

	ok, err := w.exps.RequeueInfrastructureFault(context.Background(), id, domain.StatusSubmitted, string(domain.EvictionStuckPending), 1)
	if err != nil || !ok {
		t.Fatalf("requeue = %v, %v", ok, err)
	}
	if got := w.get(t, id); len(got.TriedClusters) != 0 || got.InfraRequeueCount != 1 {
		t.Fatalf("tried list = %+v, infra %d, want no entry and one spent", got.TriedClusters, got.InfraRequeueCount)
	}
}

// The store's own termination path makes the same decision: a scale-up timeout with the
// allowance spent ends the job.
func TestAScaleUpTimeoutPastTheAllowanceEndsTheJob(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	w.bound(t, w.ofA)
	store := &Store{pool: w.exps.pool, maxInfraRequeues: 1}
	id := w.placed(t, "tried-ended", w.peA, w.ofA, domain.StatusSubmitted)

	first, err := store.ResolveTermination(ctx, id, domain.StatusSubmitted, domain.StatusEvicted, string(domain.EvictionScaleUpTimeout))
	if err != nil || first != domain.TerminationRequeued {
		t.Fatalf("first scale-up timeout = %v, %v, want requeued", first, err)
	}
	if !w.claim(t, id, w.ofA) {
		t.Fatal("the requeued job was not placed again")
	}
	second, err := store.ResolveTermination(ctx, id, domain.StatusSubmitted, domain.StatusEvicted, string(domain.EvictionScaleUpTimeout))
	if err != nil || second != domain.TerminationWritten {
		t.Fatalf("scale-up timeout past the allowance = %v, %v, want the job ended", second, err)
	}
	if got := w.get(t, id); got.Status != domain.StatusEvicted || got.EvictionReason != string(domain.EvictionScaleUpTimeout) {
		t.Fatalf("the job: %s for %q, want EVICTED for scale_up_timeout", got.Status, got.EvictionReason)
	}
}

// The backoff other jobs observe stays what it was: a cluster a job failed a scale-up on. A
// job's other infrastructure faults are for its own next placement only.
func TestOnlyScaleUpFailuresHoldOtherJobsOffACluster(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	scaled, stuck, old := w.bound(t, w.ofA), w.bound(t, w.ofB), "rid-old-"+w.suffix
	if _, _, err := w.clusters.UpdateCluster(ctx, w.ofB, sharedAs(true), domain.ClusterSettings{}); err != nil {
		t.Fatal(err)
	}
	onScaled := w.placed(t, "others-scale", w.peA, w.ofA, domain.StatusSubmitted)
	onStuck := w.placed(t, "others-stuck", w.peA, w.ofB, domain.StatusSubmitted)
	if ok, err := w.exps.RequeueInfrastructureFault(ctx, onScaled, domain.StatusSubmitted, string(domain.EvictionScaleUpTimeout), 3); err != nil || !ok {
		t.Fatalf("requeue = %v, %v", ok, err)
	}
	if ok, err := w.exps.RequeueInfrastructureFault(ctx, onStuck, domain.StatusSubmitted, string(domain.EvictionStuckPending), 3); err != nil || !ok {
		t.Fatalf("requeue = %v, %v", ok, err)
	}
	// An entry from before reasons were kept: every one of those was a scale-up failover.
	legacy := w.queued(t, "others-legacy", w.peA)
	if _, err := w.pool.pool.Exec(ctx, `UPDATE experiments SET tried_clusters = jsonb_build_array(jsonb_build_object('cluster_id', $2::text, 'at', to_jsonb(NOW()))), updated_at = NOW() WHERE id = $1`, legacy, old); err != nil {
		t.Fatal(err)
	}

	recent, err := w.exps.RecentlyTriedClusters(ctx, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !recent[scaled] || !recent[old] {
		t.Fatalf("recently tried = %v, want the cluster a scale-up failed on and the one of the entry with no reason", recent)
	}
	if recent[stuck] {
		t.Fatalf("recently tried = %v: a job stuck pending on %s holds every other job off it", recent, stuck)
	}
}

// The reason a requeue writes explains the attempt that ended. It reads while the job waits, the
// claim that starts the next attempt takes it away, and what stays is the tried list and the
// counts: a job that then ends by running is never explained by the attempt before.
func TestAReasonEndsWithTheAttemptItExplains(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	w.bound(t, w.ofA)
	id := w.placed(t, "reason-of-attempt", w.peA, w.ofA, domain.StatusSubmitted)
	if ok, err := w.exps.RequeueInfrastructureFault(ctx, id, domain.StatusSubmitted, string(domain.EvictionStuckPending), 2); err != nil || !ok {
		t.Fatalf("requeue = %v, %v", ok, err)
	}
	if got := w.get(t, id); got.EvictionReason != string(domain.EvictionStuckPending) {
		t.Fatalf("while it waits: reason %q, want stuck_pending", got.EvictionReason)
	}
	if !w.claim(t, id, w.ofA) {
		t.Fatal("the requeued job was not placed again")
	}
	got := w.get(t, id)
	if got.EvictionReason != "" || got.InfraRequeueCount != 1 || got.InfraRequeueReason != string(domain.EvictionStuckPending) || len(got.TriedClusters) != 1 {
		t.Fatalf("placed again: reason %q, infra %d (%q), tried %+v, want no reason and the history kept", got.EvictionReason, got.InfraRequeueCount, got.InfraRequeueReason, got.TriedClusters)
	}
	_, err := w.pool.pool.Exec(ctx, `UPDATE experiments SET eviction_reason = 'stuck_pending' WHERE id = $1`, id)
	if err == nil || !strings.Contains(err.Error(), "experiments_reason_of_attempt") {
		t.Fatalf("a reason written onto a placed job: err = %v, want experiments_reason_of_attempt", err)
	}
}

// The type a job ran on is its own fact, written when it is marked RUNNING. It is there however
// the attempt ended and through every later placement, one that is rolled back included, where
// the reason the attempt ended is gone with the claim. A job that was placed and never ran has
// none.
func TestTheTypeAJobRanOnOutlivesEveryRequeue(t *testing.T) {
	w := newTwoOrgs(t)
	ctx := context.Background()
	w.bound(t, w.ofA)
	never := w.placed(t, "never-ran", w.peA, w.ofA, domain.StatusSubmitted)
	if ok, err := w.exps.RequeueInfrastructureFault(ctx, never, domain.StatusSubmitted, string(domain.EvictionStuckPending), 2); err != nil || !ok {
		t.Fatalf("requeue of a job that never ran = %v, %v", ok, err)
	}
	if got := w.get(t, never); got.RanOnAcceleratorType != "" {
		t.Fatalf("a job that never ran is held to %q", got.RanOnAcceleratorType)
	}

	id := w.placed(t, "ran", w.peA, w.ofA, domain.StatusRunning)
	ranOn := w.get(t, id).AcceleratorType
	held := func(when string) {
		t.Helper()
		if got := w.get(t, id); ranOn == "" || got.RanOnAcceleratorType != ranOn {
			t.Fatalf("%s: ran on %q, want %q", when, got.RanOnAcceleratorType, ranOn)
		}
	}
	held("running")
	if ok, err := w.exps.RequeuePreempted(ctx, id, 0.5, 0.5); err != nil || !ok {
		t.Fatalf("preempt = %v, %v", ok, err)
	}
	held("preempted and waiting")
	if !w.claim(t, id, w.ofA) {
		t.Fatal("the preempted job was not placed again")
	}
	if started, err := w.exps.MarkStarted(ctx, id); err != nil || !started {
		t.Fatalf("MarkStarted = %v, %v", started, err)
	}
	if ok, err := w.exps.RequeueInfrastructureFault(ctx, id, domain.StatusRunning, string(domain.EvictionWorkloadGone), 2); err != nil || !ok {
		t.Fatalf("infrastructure requeue = %v, %v", ok, err)
	}
	held("requeued by an infrastructure fault")
}
