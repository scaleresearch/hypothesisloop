package db

import (
	"github.com/jackc/pgx/v5"

	"github.com/scaleresearch/hypothesisloop/controlplane/domain"
)

// jobWrittenColumns are what a write of an experiments row announces, as the write left it.
const jobWrittenColumns = `id, platform_experiment_id, agent_id, status, COALESCE(eviction_reason, ''),
	COALESCE(not_admitted_reason, ''), COALESCE(held_reason, ''), hypothesis_id, COALESCE(same_hypothesis, ''),
	COALESCE(underperforming_metric, '')`

const jobReturning = ` RETURNING ` + jobWrittenColumns

// jobWritten is one experiments row as read back by jobReturning.
type jobWritten struct {
	id, platformExperimentID, agentID, status, evictionReason, notAdmittedReason string
	heldReason, hypothesisID, sameHypothesis, underperformingMetric             string
}

func (j *jobWritten) dest() []any {
	return []any{&j.id, &j.platformExperimentID, &j.agentID, &j.status, &j.evictionReason,
		&j.notAdmittedReason, &j.heldReason, &j.hypothesisID, &j.sameHypothesis, &j.underperformingMetric}
}

func scanJobWritten(row pgx.Row) (jobWritten, error) {
	var j jobWritten
	err := row.Scan(j.dest()...)
	return j, err
}

func collectJobsWritten(rows pgx.Rows) ([]jobWritten, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (jobWritten, error) { return scanJobWritten(row) })
}

func (j jobWritten) change(kind, value, detail string) domain.Change {
	return domain.Change{Kind: kind, Subject: j.id, Value: value, Detail: detail,
		PlatformExperimentID: j.platformExperimentID, AgentID: j.agentID}
}

// jobStatus announces the status a write moved the job to, as a read of the job answers it, and
// the queue reason it waits for, which a job nobody has judged yet does not have.
func (w *Write) jobStatus(j jobWritten) {
	status := domain.AnsweredStatus(domain.ExperimentStatus(j.status), j.evictionReason)
	w.Change(j.change(domain.EventExperimentStatus, string(status), j.evictionReason))
	w.jobQueueReason(j)
}

// jobQueueReason announces the queue reason a write gave the job.
func (w *Write) jobQueueReason(j jobWritten) {
	reason, message := domain.SplitQueueReason(j.notAdmittedReason)
	if reason == "" || reason == domain.NotAdmittedAwaitingAdmission {
		return
	}
	w.Change(j.change(domain.EventExperimentBlocked, reason, message))
}

// jobHeld announces the reason a write recorded the job is held for.
func (w *Write) jobHeld(j jobWritten) {
	reason, message := domain.SplitQueueReason(j.heldReason)
	w.Change(j.change(domain.EventJobHeld, reason, message))
}
