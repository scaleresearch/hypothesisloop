package controller

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/metricsdb"
	"github.com/scaleresearch/hypothesisloop/controlplane/shared/workload"
)

// silenceController wires just enough controller for checkSilence: a 30s silence window
// (3 × 10s), and a 5 minute ceiling on how long a cluster may report nothing at all.
func silenceController(observed ObservedState) *Controller {
	return &Controller{
		observed:              observed,
		logger:                zap.NewNop(),
		silenceMultiplier:     3,
		defaultReportInterval: 10 * time.Second,
		minSilenceWindow:      10 * time.Second,
		clusterSilenceCeiling: 5 * time.Minute,
	}
}

// liveJob is the common starting point: a job seen 30 minutes ago, still alive, confirmed Running
// by a cluster that is reporting freshly and lists it in its newest snapshot. Every check before
// the declared-metric contract passes for it, so a test can set the one field it is about.
func liveJob() fakeObserved {
	return fakeObserved{
		firstObserved: time.Now().UTC().Add(-30 * time.Minute),
		observedOK:    true,
		alive:         true,
		phase:         workload.JobPhaseRunning,
		phaseFound:    true,
		presence:      metricsdb.SnapshotPresence{Reported: true, SnapshotAge: 5 * time.Second},
	}
}

func runningExperiment() *domain.Experiment {
	return &domain.Experiment{
		ID:                   "exp-mute",
		ClusterName:          "cluster-a",
		PlatformExperimentID: "pe-1",
		Status:               domain.StatusRunning,
	}
}

// silence runs checkSilence with one declared metric and fails on a query error, which is the
// shape almost every case below wants.
func silence(t *testing.T, observed fakeObserved, keys ...string) (bool, domain.EvictionReason) {
	t.Helper()
	evict, reason, err := silenceController(observed).checkSilence(
		context.Background(), runningExperiment(), time.Now().UTC(), nil, keys)
	if err != nil {
		t.Fatalf("checkSilence: %v", err)
	}
	return evict, reason
}

const declared = "val_accuracy"

// A live job that has never emitted a metric its platform experiment declared cannot be ranked,
// cut or compared — there is nothing to judge it by — while it holds an accelerator and bills for
// it. It is evicted, and named for the actual fault: its reporting path, not a hung trainer.
func TestCheckSilenceEvictsLiveJobThatNeverReportedADeclaredMetric(t *testing.T) {
	obs := liveJob() // nothing in the window, nothing ever

	evict, reason := silence(t, obs, declared)
	if !evict {
		t.Fatal("a live job that never reported its declared metric was not evicted — nothing can judge it, and it bills for an accelerator meanwhile")
	}
	if reason != domain.EvictionNeverReportedMetrics {
		t.Fatalf("eviction reason = %q, want %q: the fault is the reporting path, not a hung trainer",
			reason, domain.EvictionNeverReportedMetrics)
	}
}

// The trap this verdict must not fall into: "no samples in the silence window" and "never
// reported at all" are different jobs. One reported an hour ago and went quiet — a hung trainer,
// judged elsewhere — and calling that "never reported" would both evict on the wrong evidence and
// tell the agent to go fix a reporting path that works.
func TestCheckSilenceDoesNotCallAQuietJobNeverReported(t *testing.T) {
	obs := liveJob()
	obs.declaredEverReported = true // nothing lately, but it reported earlier in its life

	if _, reason := silence(t, obs, declared); reason == domain.EvictionNeverReportedMetrics {
		t.Fatal("a job that reported earlier and went quiet was condemned as never having reported")
	}
}

// The single-sample case, which the progress reader deliberately folds into "not reported": one
// point cannot prove movement. It IS a complete answer to "did this job ever report", though, so a
// job that emitted exactly one metric must not be condemned as never having reported. That is the
// whole reason the lifetime question is asked with a different reader than the in-window one.
func TestCheckSilenceDoesNotCallAJobWithOneSampleNeverReported(t *testing.T) {
	obs := liveJob()
	obs.declaredReported = false    // one point does not show movement...
	obs.declaredEverReported = true // ...but it is proof the path works

	if _, reason := silence(t, obs, declared); reason == domain.EvictionNeverReportedMetrics {
		t.Fatal("a job that emitted exactly one declared metric was condemned as never having reported")
	}
}

// A job whose declared metric keeps arriving but never moves is a hung training loop re-emitting a
// cached value. It looks perfectly alive to any presence-only check, which is why the metric
// contract exists.
func TestCheckSilenceEvictsAJobWhoseDeclaredMetricStoppedMoving(t *testing.T) {
	obs := liveJob()
	obs.declaredReported = true // reporting steadily...
	obs.declaredChanged = false // ...at one constant value

	evict, reason := silence(t, obs, declared)
	if !evict {
		t.Fatal("a job re-emitting one constant value was not evicted — a hung trainer looks alive forever")
	}
	if reason != domain.EvictionSilent {
		t.Fatalf("eviction reason = %q, want %q: it reported, so the fault is the trainer, not the reporting path",
			reason, domain.EvictionSilent)
	}
}

// A job whose declared metric is arriving and moving is working, however little it has produced.
func TestCheckSilenceSparesAJobWhoseDeclaredMetricIsMoving(t *testing.T) {
	obs := liveJob()
	obs.declaredReported = true
	obs.declaredChanged = true

	if evict, _ := silence(t, obs, declared); evict {
		t.Fatal("a job reporting a moving declared metric was evicted")
	}
}

// A platform experiment declaring no ranking metric has nothing to hold a job to, so silence
// detection has no contract to enforce and must not invent one.
func TestCheckSilenceSparesJobWhenNoMetricWasDeclared(t *testing.T) {
	if evict, _ := silence(t, liveJob()); evict {
		t.Fatal("a job was evicted for not reporting a metric its platform experiment never declared")
	}
}

// Nothing has ever been seen from this job, so there is no start instant to measure silence from
// and nothing to conclude. Judging it here would condemn every job in the gap between admission
// and its first report.
func TestCheckSilenceDeclinesToJudgeAJobNothingHasBeenSeenFrom(t *testing.T) {
	obs := liveJob()
	obs.observedOK = false

	if evict, _ := silence(t, obs, declared); evict {
		t.Fatal("a job with no observation at all was evicted — there is no evidence either way about it yet")
	}
}

// Silence is only silence once a full window has passed. A job seen for the first time seconds ago
// has not yet had the chance to report that the window is measuring.
func TestCheckSilenceSparesAJobYoungerThanOneWindow(t *testing.T) {
	obs := liveJob()
	obs.firstObserved = time.Now().UTC().Add(-2 * time.Second) // window is 30s
	obs.alive = false                                          // would otherwise be a verdict

	if evict, _ := silence(t, obs, declared); evict {
		t.Fatal("a job evicted before one silence window had even elapsed — it never got the chance to report")
	}
}

// The cluster itself has stopped reporting. Whether this job is alive is unknowable and stays
// unknowable until reporting resumes, so waiting buys no better evidence — only a growing pile of
// reservations held against a cluster nobody can see.
func TestCheckSilenceEvictsWhenTheClusterIsNotReportingAtAll(t *testing.T) {
	obs := liveJob()
	obs.phaseFound = false
	obs.presence = metricsdb.SnapshotPresence{Reported: false}

	evict, reason := silence(t, obs, declared)
	if !evict || reason != domain.EvictionClusterUnreachable {
		t.Fatalf("evict = %v reason = %q, want an eviction as %q: nothing can be learned about a job on a cluster that has gone dark",
			evict, reason, domain.EvictionClusterUnreachable)
	}
}

// Same verdict by the other route: the cluster is technically reporting, but its newest snapshot
// is older than the outage the deployment tolerates.
func TestCheckSilenceEvictsWhenTheClustersNewestSnapshotIsPastTheCeiling(t *testing.T) {
	obs := liveJob()
	obs.phaseFound = false
	obs.presence = metricsdb.SnapshotPresence{Reported: true, SnapshotAge: 6 * time.Minute} // ceiling is 5

	evict, reason := silence(t, obs, declared)
	if !evict || reason != domain.EvictionClusterUnreachable {
		t.Fatalf("evict = %v reason = %q, want an eviction as %q past the cluster silence ceiling",
			evict, reason, domain.EvictionClusterUnreachable)
	}
}

// A fresh cluster that simply has no report for this job inside the silence window is not a verdict
// on the job. Evicting here would destroy running work on a reporting-cadence artefact.
func TestCheckSilenceWaitsWhenAFreshClusterHasNoReportForTheJob(t *testing.T) {
	obs := liveJob()
	obs.phaseFound = false
	obs.presence = metricsdb.SnapshotPresence{Reported: true, SnapshotAge: 3 * time.Second}

	if evict, reason := silence(t, obs, declared); evict {
		t.Fatalf("evicted as %q on a fresh cluster that just had no report for this job in the window", reason)
	}
}

// Missing from the newest snapshot but present in the one before it is what a routine
// delete-then-recreate looks like from here, and it resolves itself within a snapshot or two.
// Acting on the first absence turns a normal reschedule into a terminated experiment.
func TestCheckSilenceWaitsForAnAbsenceToBeConfirmedAcrossSnapshots(t *testing.T) {
	obs := liveJob()
	obs.phase = workload.JobPhaseGone
	obs.presence = metricsdb.SnapshotPresence{
		Reported:        true,
		SnapshotAge:     3 * time.Second,
		AbsentSnapshots: metricsdb.GoneConfirmingSnapshots - 1,
	}

	if evict, reason := silence(t, obs, declared); evict {
		t.Fatalf("evicted as %q on a single unconfirmed absence — a delete-then-recreate looks exactly like this and resolves itself",
			reason)
	}
}

// Consecutive complete snapshots from a live cluster, none mentioning this job: the runtime's own
// confirmation that no pod exists for it. Unlike a Pending job nothing here converges by waiting,
// and the quota stays stranded until someone notices.
func TestCheckSilenceEvictsAWorkloadTheClusterConfirmsIsGone(t *testing.T) {
	obs := liveJob()
	obs.phase = workload.JobPhaseGone
	obs.presence = metricsdb.SnapshotPresence{
		Reported:        true,
		SnapshotAge:     3 * time.Second,
		AbsentSnapshots: metricsdb.GoneConfirmingSnapshots,
	}

	evict, reason := silence(t, obs, declared)
	if !evict || reason != domain.EvictionWorkloadGone {
		t.Fatalf("evict = %v reason = %q, want an eviction as %q once the absence is confirmed",
			evict, reason, domain.EvictionWorkloadGone)
	}
}

// A job between pods is expected to be quiet — that is a reschedule in progress, not a dead
// trainer. This is the case the phase check exists for.
func TestCheckSilenceSparesAJobThatIsNotYetRunning(t *testing.T) {
	obs := liveJob()
	obs.phase = workload.JobPhasePending
	obs.alive = false

	if evict, reason := silence(t, obs, declared); evict {
		t.Fatalf("evicted as %q while the pod was Pending — quiet is expected mid-reschedule", reason)
	}
}

// Silence from a job that is no longer alive has two causes that must not be conflated, because
// the reason is what tells the agent where to look. One reported and stopped: the trainer died.
func TestCheckSilenceNamesADeadTrainerSilentWhenItHadReported(t *testing.T) {
	obs := liveJob()
	obs.alive = false
	obs.everReportedAny = true

	evict, reason := silence(t, obs, declared)
	if !evict || reason != domain.EvictionSilent {
		t.Fatalf("evict = %v reason = %q, want %q: this job's reporting path demonstrably worked",
			evict, reason, domain.EvictionSilent)
	}
}

// ...and one that never reported at all has a reporting path that never worked. Workloads
// typically swallow the post failure and only warn to stderr, so this reaches an operator as a
// "stuck job" with no hint the metrics path is at fault unless the reason says so.
func TestCheckSilenceNamesADeadJobThatNeverReportedForItsReportingPath(t *testing.T) {
	obs := liveJob()
	obs.alive = false
	obs.everReportedAny = false

	evict, reason := silence(t, obs, declared)
	if !evict || reason != domain.EvictionNeverReportedMetrics {
		t.Fatalf("evict = %v reason = %q, want %q: nothing was ever posted, so the fault is the reporting path",
			evict, reason, domain.EvictionNeverReportedMetrics)
	}
}

// An unanswerable query is not evidence of silence. Every read checkSilence makes must surface its
// error rather than resolve to a verdict, or a metrics-store outage evicts the whole platform.
func TestCheckSilenceNeverEvictsOnAQueryItCouldNotAnswer(t *testing.T) {
	for _, method := range []string{
		"FirstObserved", "LatestJobPhase", "ClusterSnapshotPresence",
		"IsAlive", "AnyDeclaredMetricChanged", "AnyDeclaredMetricReported", "HasEverReportedMetric",
	} {
		t.Run(method, func(t *testing.T) {
			obs := liveJob()
			obs.failOn = method
			// Steer into the branch that makes this particular read happen.
			switch method {
			case "ClusterSnapshotPresence":
				obs.phaseFound = false
			case "HasEverReportedMetric":
				obs.alive = false
			}

			evict, _, err := silenceController(obs).checkSilence(
				context.Background(), runningExperiment(), time.Now().UTC(), nil, []string{declared})
			if err == nil {
				t.Fatalf("a failed %s was swallowed — the caller cannot tell an unanswered question from a clean answer", method)
			}
			if evict {
				t.Fatalf("evicted on a failed %s: a metrics-store outage would terminate every running job", method)
			}
		})
	}
}
