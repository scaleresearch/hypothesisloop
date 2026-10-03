package scheduler

import (
	"fmt"
	"strings"

	"github.com/scaleresearch/hypothesisloop/controlplane/shared/domain"
)

// Every not_admitted_reason is stored as "code: sentence" (domain.SplitQueueReason). The sentence
// is what job.held and not_admitted_message carry, so a reason written without one tells the
// job's owner nothing. The reasons below are the ones whose sentence needs little of the job; the
// rest are worded where they are decided.

// summaryNamed is how many jobs the summary gate's sentence names before it counts the rest.
const summaryNamed = 3

// summaryGateReason names the COMPLETED jobs whose summary the agent owes and the command that
// files one. owed is never empty.
func summaryGateReason(owed []string) string {
	named := owed[:min(len(owed), summaryNamed)]
	subject := "job " + named[0] + " of this agent is COMPLETED and has no summary"
	if len(owed) > 1 {
		subject = "jobs " + strings.Join(named, ", ")
		if rest := len(owed) - len(named); rest > 0 {
			subject += fmt.Sprintf(" and %d more", rest)
		}
		subject += " of this agent are COMPLETED and have no summary"
	}
	return fmt.Sprintf("%s: %s: hl job summary %s --text T files one, and this job is admitted once none is owed",
		domain.NotAdmittedSummaryGate, subject, named[0])
}

func jobTooLongReason(estimatedHours, limitHours float64) string {
	return fmt.Sprintf("%s: its estimated_duration_hours %g is over the %g h one job may take in the platform experiment's current stage: cancel it and submit shorter runs, or it waits for a stage that allows its length",
		domain.NotAdmittedJobTooLong, estimatedHours, limitHours)
}

func stageCutReason() string {
	return domain.NotAdmittedStageCut + ": its agent was cut at a stage of the platform experiment, and a cut agent's queued jobs are not started"
}

func waitingForScaleUpReason(cluster string) string {
	return fmt.Sprintf("%s: cluster %s was asked for more machines of this type and they have not joined yet: the job is admitted when they do", domain.NotAdmittedWaitingForScaleUp, cluster)
}

func noScalableCapacityReason() string {
	return domain.NotAdmittedNoScalableCapacity + ": no cluster has room for it now, and every cluster that scales up was asked for it lately and brought no machine: it is tried again as room frees or those tries expire"
}

func noClusterForOrgReason() string {
	return domain.NotAdmittedNoClusterForOrg + ": no cluster takes this org's jobs, none is its own and none is shared: an operator registers or shares one, and no change to the job helps"
}

func clustersDisabledReason() string {
	return domain.NotAdmittedClustersDisabled + ": the org turned off every cluster it could use: turning one on admits the job, and no change to the job helps"
}

func noMultiNodeClusterReason(nodes int) string {
	return fmt.Sprintf("%s: the job spans %d nodes, and no cluster that takes this org's jobs runs a job on more than one", domain.NotAdmittedNoMultiNodeCluster, nodes)
}

func concurrencyCapReason() string {
	return domain.NotAdmittedConcurrencyCap + ": the platform experiment has as many accelerators in flight as its max_concurrent_accelerators allows: the job is admitted as its running jobs end"
}

func capacityTakenReason() string {
	return domain.NotAdmittedCapacityUnavailable + ": another job took the room while this one was being admitted: it is looked at again within seconds"
}

func workloadCreationReason() string {
	return domain.NotAdmittedWorkloadCreation + ": the platform could not hand the job to its cluster: it is tried again within seconds"
}
