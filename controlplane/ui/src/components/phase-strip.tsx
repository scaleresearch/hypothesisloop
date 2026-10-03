'use client'

import { formatDate } from '@/lib/format'
import { PHASES, phaseOf, reasonOf } from '@/lib/phase'
import type { Draft } from '@/lib/planning'
import { useRoster } from '@/lib/roster'
import { ExperimentStatus } from '@/types'
import type { Experiment, JoinedAgent } from '@/types'
import { AgentAtWork } from '@/components/agent-at-work'
import { JobReason } from '@/components/job-reason'
import { Card, CardContent } from '@/components/ui/card'
import { TextLink } from '@/components/ui/link'
import { Steps } from '@/components/ui/steps'
import { Eyebrow } from '@/components/ui/text'

const planningEnds = (pe: Draft) => `It has until ${formatDate(pe.planning_ends_at)}; a draft with no proposal by then is closed. This page updates by itself.`

// The states this page has words of its own for, by status_reason.reason. Any other is said in
// the API's message, which is worded for whoever reads it.
const SENTENCE: Record<string, (pe: Draft) => string> = {
  planning: pe => `The planner is working out how to measure this. ${planningEnds(pe)}`,
  revising: pe => `The planner is changing its proposal. ${planningEnds(pe)}`,
  proposed: pe => `The planner proposed the experiment below. Nothing runs and nothing is spent until it is accepted, and the draft is closed on ${formatDate(pe.proposal_expires_at)} if nobody does.`,
  declined: () => 'The planner proposed no experiment and says why below.',
  agents_starting: pe => `The researchers' machines are starting. The run's clock starts when the first one is running, and the platform closes the experiment if none is by ${formatDate(pe.agents_due_at)}.`,
  running: pe => `The researchers are at work until ${formatDate(pe.ends_at)}.`,
}

function sentenceOf(pe: Draft): string {
  const { reason, message } = reasonOf(pe)
  return SENTENCE[reason]?.(pe) ?? message
}

/** Where the baseline agent's job stands, in a line of its own. */
function BaselineJob({ job }: { job: Experiment | undefined }) {
  if (!job) return <p className="text-xs text-muted">It has submitted no job yet.</p>
  const link = <TextLink inline href={`/jobs/${job.id}`}>Its job</TextLink>
  const value = job.metric ? `: ${job.metric.name} ${job.metric.value}` : ''
  switch (job.status) {
    case ExperimentStatus.SUBMITTED:
    case ExperimentStatus.QUEUED: {
      const why = job.not_admitted_message ?? job.not_admitted_reason
      return <p className="text-xs">{link} is queued{why ? `: ${why}` : '.'}</p>
    }
    case ExperimentStatus.RUNNING:
      return <p className="text-xs">{link} is running{value}.</p>
    case ExperimentStatus.COMPLETED:
      return <p className="text-xs">{link} is done{value}. The platform declares it the baseline and starts the run once the agent finishes.</p>
    default:
      return (
        <div className="text-xs">
          {link} ended {job.status.toLowerCase()}.
          {job.eviction_reason && <JobReason reason={job.eviction_reason} />}
        </div>
      )
  }
}

// The baseline agent now measuring, or the last one that did.
function baselineAgent(agents: JoinedAgent[] | undefined): JoinedAgent | undefined {
  const baselines = (agents ?? []).filter(agent => agent.role === 'baseline')
  return baselines.find(agent => !agent.finished_at) ?? baselines.at(-1)
}

/**
 * Where an experiment stands on its way from a sentence to its close, at the top of its page in
 * every state: the phase, why it is in it, and what the one agent that phase waits for is doing.
 * `jobs` are the experiment's newest, among which the baseline agent's is looked for.
 */
export function PhaseStrip({ pe, jobs = [] }: { pe: Draft; jobs?: Experiment[] }) {
  const phase = phaseOf(pe)
  const { data: agents } = useRoster(pe)
  const planner = agents?.find(agent => agent.flavor === 'planner')
  const baseline = baselineAgent(agents)
  return (
    <Card role="region" aria-label="Where this experiment stands">
      <CardContent className="grid gap-2.5">
        <Steps label="Phase" steps={PHASES} current={phase} live={phase !== 'closed'} />
        <p role="status" className="text-sm">
          {phase === 'baseline' && !pe.baseline_job_id && 'Preparing the baseline. No researcher starts and none of their machines is rented until it is measured. '}
          {sentenceOf(pe)}
        </p>
        {phase === 'planning' && (
          <>
            <AgentAtWork agent={planner} />
            {planner?.instance?.stopped && <p className="text-xs text-muted">Abandon this draft and try once more.</p>}
          </>
        )}
        {phase === 'baseline' && baseline && (
          <div role="group" aria-label="Baseline agent" className="grid gap-1.5 rounded border bg-surface-inset px-3 py-2.5">
            <Eyebrow>Baseline agent {baseline.name}</Eyebrow>
            <AgentAtWork agent={baseline} />
            <BaselineJob job={jobs.find(job => job.agent_id === baseline.agent_id)} />
          </div>
        )}
      </CardContent>
    </Card>
  )
}
