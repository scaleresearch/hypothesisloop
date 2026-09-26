package db

import (
	"context"
	"testing"
	"time"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
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
	store := &Store{ExperimentsStore: w.exps, maxInfraRequeues: 1}
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
	if _, _, err := w.clusters.SetClusterShared(ctx, w.ofB, true); err != nil {
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
	if _, err := w.pool.pool.Exec(ctx, `UPDATE experiments SET tried_clusters = jsonb_build_array(jsonb_build_object('cluster_id', $2::text, 'at', to_jsonb(NOW()))) WHERE id = $1`, legacy, old); err != nil {
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
