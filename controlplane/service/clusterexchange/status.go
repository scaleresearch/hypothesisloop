package clusterexchange

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/scaleresearch/hypothesisloop/contract/clusterapi"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
	"github.com/scaleresearch/hypothesisloop/controlplane/storage/metricsdb"
)

// PushStatus validates one complete job-phase snapshot of clusterName and writes it atomically:
// any invalid report rejects the whole push.
func (s *Service) PushStatus(ctx context.Context, clusterName string, push clusterapi.StatusPush) error {
	now := time.Now().UTC()
	statusSamples := make([]metricsdb.JobStatusSample, 0, len(push.Reports))
	// A runtime reporting a job as running is an observation that it is alive, and it is the
	// one liveness observation every runtime produces — the per-pod cgroup heartbeat exists
	// only where a node-agent is deployed. Recording it here, once, is what gives runtimes
	// without one (bare metal) any observed runtime at all: without it their jobs were billed
	// only in slivers around whatever metrics they happened to report, their preemption
	// rescale computed a stint of ~0 and requeued them at full estimate, and every rule keyed
	// on "never observed" could not be bounded because it would have condemned all of them.
	var observations []metricsdb.Observation
	ids := make([]string, 0, len(push.Reports))
	for _, rep := range push.Reports {
		if rep.ExperimentID == "" || rep.Phase == "" {
			return refusal.Reject(http.StatusBadRequest, "every status report requires experiment_id and phase")
		}
		if rep.AdmittedMachine != nil && rep.AdmittedNode == "" {
			return refusal.Reject(http.StatusBadRequest, "status report for "+rep.ExperimentID+" names an admitted_machine and no admitted_node")
		}
		if (rep.StartedAt == nil) != (rep.EndedAt == nil) || (rep.StartedAt != nil && rep.EndedAt.Before(*rep.StartedAt)) {
			return refusal.Reject(http.StatusBadRequest, "status report for "+rep.ExperimentID+" needs started_at and ended_at together, in that order")
		}
		ids = append(ids, rep.ExperimentID)
	}
	placementByID, err := s.store.PlacementByID(ctx, ids)
	if err != nil {
		return auth.Internal(s.logger, "cluster push status", err, zap.String("cluster", clusterName))
	}
	accepted := make([]clusterapi.StatusReport, 0, len(push.Reports))
	var ended []string
	for _, rep := range push.Reports {
		// A report for an experiment this cluster no longer owns is dropped, not a reason to
		// reject the push. Mid-reschedule that is the ordinary state — the experiment has
		// already been repointed at its new cluster while the old one is still tearing its
		// workload down — and rejecting the whole snapshot blacked out status for every other
		// job on the old cluster until it finished. Dropping it is also the honest report:
		// the experiment genuinely is not this cluster's any more.
		if reason, ok := rejectReport(rep, placementByID[rep.ExperimentID], clusterName); !ok {
			s.logger.Info(reason, zap.String("experiment", rep.ExperimentID),
				zap.String("cluster", clusterName), zap.Int("reported_attempt", reportedAttempt(rep)),
				zap.Int("current_attempt", placementByID[rep.ExperimentID].AttemptCount))
			continue
		}
		accepted = append(accepted, rep)
		if rep.EndedAt != nil {
			ended = append(ended, rep.ExperimentID)
		}
	}
	// A runtime reports an ended run on every push until its workload is deleted. The run is
	// placed by the time of the first of them, so the later ones write the same observations.
	runReportedAt, err := s.store.RunFirstReportedAt(ctx, ended)
	if err != nil {
		return auth.Internal(s.logger, "cluster push status", err, zap.String("cluster", clusterName))
	}
	for _, rep := range accepted {
		statusSamples = append(statusSamples, metricsdb.JobStatusSample{
			ExperimentID: rep.ExperimentID, ClusterName: clusterName, Phase: rep.Phase,
			AdmittedAcceleratorType: rep.AdmittedAcceleratorType, AdmittedNode: rep.AdmittedNode, AdmittedMachine: rep.AdmittedMachine, At: now,
		})
		observations = append(observations, observationsOf(rep, placementByID[rep.ExperimentID].AdmittedAt, runReportedAt[rep.ExperimentID], clusterName, now, s.observationCadence)...)
		// LogTail is optional per report (nil when the executor has nothing new)
		// -- only write when the agent actually sent one, so a
		// job with no fresh output this tick doesn't wipe out its last-known tail.
		//
		// A tail or a phase detail that cannot be written fails the push before any phase is
		// recorded: the phase that ends a job is what its workload is deleted on, and the
		// reason it ended would be gone with it. The runtime pushes the snapshot again.
		if rep.LogTail != nil {
			if err := s.metrics.RecordLogTail(ctx, rep.ExperimentID, clusterName, splitLongLines(rep.LogTail, s.maxLogLineChars), now); err != nil {
				return auth.Internal(s.logger, "cluster push status", err, zap.String("experiment_id", rep.ExperimentID), zap.String("cluster", clusterName))
			}
		}
		// Reason/Message/RestartCount are optional the same way: only write when the
		// runtime actually has something to say about why this job's container hasn't
		// started or has been restarting, so a healthy tick doesn't overwrite a still-valid
		// prior explanation with silence.
		if rep.Reason != "" || rep.Message != "" || rep.RestartCount != 0 || rep.ScheduledNodes != 0 || rep.SchedulingReason != "" {
			if err := s.metrics.RecordPhaseDetail(ctx, rep.ExperimentID, clusterName, rep.Reason, rep.Message, rep.RestartCount, rep.ScheduledNodes, rep.SchedulingReason, placementByID[rep.ExperimentID].AttemptCount, now); err != nil {
				return auth.Internal(s.logger, "cluster push status", err, zap.String("experiment_id", rep.ExperimentID), zap.String("cluster", clusterName))
			}
		}
	}
	// Observations first: the job watcher settles a job as soon as it reads the phase that
	// ended it, from whatever was observed of it by then.
	if len(observations) > 0 {
		if err := s.metrics.RecordObservations(ctx, observations); err != nil {
			return auth.Internal(s.logger, "cluster push status", err, zap.String("cluster", clusterName))
		}
	}
	if err := s.metrics.RecordJobStatuses(ctx, clusterName, now, statusSamples); err != nil {
		return auth.Internal(s.logger, "cluster push status", err, zap.String("cluster", clusterName))
	}
	return nil
}

// measuredRun places the run a machine timed by its own clock on this one. How long the command
// ran is the machine's word, since a clock that is wrong is wrong at both ends alike. When it ran
// is held between the job's admission and the first report of its end, which is all the platform
// saw of it: a run told by a clock that is ahead ends at that report, one told by a clock that is
// behind starts at the admission, and none is longer than the time between the two. Both bounds
// are stored, so every report of one run places it the same.
func measuredRun(startedAt, endedAt, admittedAt, reportedAt time.Time) (from, to time.Time) {
	ran := min(endedAt.Sub(startedAt), reportedAt.Sub(admittedAt))
	to = endedAt
	if to.After(reportedAt) {
		to = reportedAt
	}
	if to.Add(-ran).Before(admittedAt) {
		to = admittedAt.Add(ran)
	}
	return to.Add(-ran), to
}

// observationsOf is when rep shows its job alive: now for a job reported running, and the whole
// of the run its runtime measured, as observations no further apart than cadence, so that the
// span every reader computes (metricsdb.ObserveSpan) holds the run whole however rarely the job
// was seen running. A run reported for a job that holds no capacity is nobody's. runReportedAt is
// when the end of the run was first reported (Store.RunFirstReportedAt).
func observationsOf(rep clusterapi.StatusReport, admittedAt, runReportedAt time.Time, clusterName string, now time.Time, cadence time.Duration) []metricsdb.Observation {
	labels := map[string]string{"cluster_name": clusterName}
	if rep.AdmittedNode != "" {
		labels["node"] = rep.AdmittedNode
	}
	at := func(t time.Time) metricsdb.Observation {
		return metricsdb.Observation{ExperimentID: rep.ExperimentID, At: t.UTC(), ExtraLabels: labels}
	}
	var observations []metricsdb.Observation
	if rep.Phase == metricsdb.PhaseRunning {
		observations = append(observations, at(now))
	}
	if rep.EndedAt == nil || admittedAt.IsZero() || !runReportedAt.After(admittedAt) {
		return observations
	}
	from, to := measuredRun(*rep.StartedAt, *rep.EndedAt, admittedAt, runReportedAt)
	for t := from; t.Before(to); t = t.Add(cadence) {
		observations = append(observations, at(t))
	}
	return append(observations, at(to))
}

// rejectReport decides whether one status report describes the workload the control plane is
// actually waiting on. ok=false means drop it — never reject the whole push: mid-reschedule a
// stale report is the ordinary state, and failing the snapshot blacks out status for every other
// job on the cluster until it settles.
//
// Two things have to match. Ownership, because the experiment may already have been repointed at
// another cluster while this one is still tearing its workload down. And generation, because
// ownership alone does not cover a retry that lands back on the cluster it just failed on: the
// previous attempt's workload is still terminating and still in this snapshot
// (ListManagedJobsForStatus includes terminating workloads on purpose), so the Failed that caused
// the requeue would be recorded as the observation of the attempt that just started, and fail it
// again immediately.
//
// AttemptUnknown is accepted rather than dropped. Every cluster-agent sends it for a job it holds
// no workload for (phase gone), which has no generation to name, so absence must survive decoding
// as absence rather than as attempt 0.
//
// A job admitted to a slot has a third thing to match, the slot. A preempted job keeps its
// attempt, so readmitted to another slot of the cluster it was preempted on, the workload it left
// behind is of the right cluster and the right generation, and its exit would be recorded as the
// readmitted job's result. A report naming no node is accepted: it describes no workload in
// particular.
func rejectReport(rep clusterapi.StatusReport, placement domain.Placement, clusterName string) (string, bool) {
	if placement.ClusterName != clusterName {
		return "dropping status report for an experiment this cluster no longer owns", false
	}
	if attempt := reportedAttempt(rep); attempt != domain.AttemptUnknown && attempt != placement.AttemptCount {
		return "dropping status report from a superseded attempt", false
	}
	if placement.Slot != "" && rep.AdmittedNode != "" && rep.AdmittedNode != placement.Slot {
		return "dropping status report from a slot the experiment is no longer placed on", false
	}
	return "", true
}

// reportedAttempt is the generation a report belongs to, or domain.AttemptUnknown when the
// cluster-agent named none.
func reportedAttempt(rep clusterapi.StatusReport) int {
	if rep.Attempt == nil {
		return domain.AttemptUnknown
	}
	return *rep.Attempt
}
