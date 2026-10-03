'use client'

import { useState } from 'react'
import { useSWRConfig } from 'swr'
import { formatDate } from '@/lib/format'
import { OWNER_ACTIONS, PHASES, phaseOf, reasonOf, waitsFor } from '@/lib/phase'
import type { Draft } from '@/lib/planning'
import { useRoster } from '@/lib/roster'
import { ExperimentStatus } from '@/types'
import type { Experiment, JoinedAgent, StatusReason } from '@/types'
import { actorLabel } from '@/components/actor'
import { AgentAtWork } from '@/components/agent-at-work'
import { AGENTS_CARD } from '@/components/experiment-agents'
import { JobReason } from '@/components/job-reason'
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { TextLink, textLinkClass } from '@/components/ui/link'
import { Callout } from '@/components/ui/status-message'
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
  researchers_done: pe => `No researcher is left to submit a job: each one finished, was cut or lost its machine. The platform closes the experiment on ${formatDate(pe.ends_at)}. Before then its summary is written with \`hl platform-experiments summary --file FILE --platform-experiment ${pe.id}\` and it is closed with \`hl platform-experiments close --platform-experiment ${pe.id}\`.`,
  end_time_reached: () => 'It reached its end time.',
  closed_by_request: pe => `It was closed early by ${actorLabel(pe.closed_by!)}.`,
  planning_limit_reached: () => 'It never ran: its planner proposed nothing within the planning limit.',
  proposal_expired: () => 'It never ran: nobody took up its planner\'s answer in time.',
  baseline_limit_reached: () => 'It never ran: no baseline was measured in time.',
  planner_stopped: () => 'It never ran: its planner\'s machine ended before it answered, as often as the platform restarts one. Nothing was spent on a run, and the same sentence makes a new draft.',
  agents_not_started: () => 'It started, and no researcher\'s machine was running in time, so the platform closed it. None of its duration was used. Agents, below, says why each machine did not come up.',
}

function sentenceOf(pe: Draft): string {
  const { reason, message } = reasonOf(pe)
  return SENTENCE[reason]?.(pe) ?? message
}

/** The control for what an experiment waits on its owner for, where this page has one. */
function OwnerMove({ pe }: { pe: Draft }) {
  const { mutate } = useSWRConfig()
  const [editing, setEditing] = useState(false)
  const { reason } = reasonOf(pe)
  if (!(reason in OWNER_ACTIONS)) throw new Error(`status reason ${reason} waits for the owner and the page has no entry for it`)
  const action = OWNER_ACTIONS[reason]
  if (!action) return null
  if (action.opens === 'agents') return <a className={textLinkClass} href={`#${AGENTS_CARD}`}>{action.label}</a>
  return (
    <>
      <Button size="sm" onClick={() => setEditing(true)}>{action.label}</Button>
      {editing && <PlatformExperimentDialog initial={pe} onClose={() => setEditing(false)} onSaved={() => void mutate(['pe', pe.id])} />}
    </>
  )
}

const NEXT_IS: Record<Exclude<StatusReason['waits_for'], 'owner' | undefined>, string> = {
  coordinator: 'its coordinator acts next',
  platform: 'the platform acts next',
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
 * every state: the phase, why it is in it, whose move it is, and what the one agent that phase
 * waits for is doing. `jobs` are the experiment's newest, among which the baseline agent's is
 * looked for. A reader of a shared experiment is told neither that a move is theirs nor that none is.
 */
export function PhaseStrip({ pe, jobs = [], readOnly = false }: { pe: Draft; jobs?: Experiment[]; readOnly?: boolean }) {
  const phase = phaseOf(pe)
  const why = (
    <p role="status" className="text-sm">
      {phase === 'baseline' && !pe.baseline_job_id && 'Preparing the baseline. No researcher starts and none of their machines is rented until it is measured. '}
      {phase === 'closed' && pe.closed_at && `Closed ${formatDate(pe.closed_at)}. `}
      {sentenceOf(pe)}
    </p>
  )
  // A closed experiment waits for nobody, and a reader is told neither whose move it is.
  const who = readOnly || phase === 'closed' ? undefined : waitsFor(pe)
  const { data: agents } = useRoster(pe)
  const planner = agents?.find(agent => agent.flavor === 'planner')
  const baseline = baselineAgent(agents)
  return (
    <Card role="region" aria-label="Where this experiment stands">
      <CardContent className="grid gap-2.5">
        <Steps label="Phase" steps={PHASES} current={phase} live={phase !== 'closed'} />
        {!who ? why : who === 'owner' ? (
          <Callout tone="warning" className="mb-0 grid justify-items-start gap-1.5">
            <Eyebrow tone="warning">Your move</Eyebrow>
            {why}
            <OwnerMove pe={pe} />
          </Callout>
        ) : (
          <div>
            {why}
            <p className="text-xs text-muted">Nothing is needed from you: {NEXT_IS[who]}.</p>
          </div>
        )}
        {phase === 'planning' && <AgentAtWork agent={planner} activity={pe.activity} />}
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
