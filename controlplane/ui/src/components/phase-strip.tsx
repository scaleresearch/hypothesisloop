'use client'

import { useState } from 'react'
import { useSWRConfig } from 'swr'
import { formatDate } from '@/lib/format'
import { GITHUB_CARD } from '@/lib/github-connect'
import { PHASES, phaseOf, reasonOf } from '@/lib/phase'
import type { Draft } from '@/lib/planning'
import { useRoster } from '@/lib/roster'
import { ExperimentStatus, PlatformExperimentStatus } from '@/types'
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
  end_time_reached: () => 'It reached its end time.',
  closed_by_request: pe => `It was closed early by ${actorLabel(pe.closed_by!)}.`,
  planning_limit_reached: () => 'It never ran: its planner proposed nothing within the planning limit.',
  proposal_expired: () => 'It never ran: nobody took up its planner\'s answer in time.',
  baseline_limit_reached: () => 'It never ran: no baseline was measured in time.',
  agents_not_started: () => 'It started, and no researcher\'s machine was running in time, so the platform closed it. None of its duration was used. Agents, below, says why each machine did not come up.',
}

function sentenceOf(pe: Draft): string {
  const { reason, message } = reasonOf(pe)
  return SENTENCE[reason]?.(pe) ?? message
}

// What the org's GitHub connection lacks, as the API names it where a repository is needed.
const GITHUB_REASONS = new Set(['github_not_connected', 'installation_not_connected', 'repository_authorization_required', 'repository_not_reachable'])

/** The control for what an experiment waits on its owner for, where this page has one. */
function OwnerAction({ pe }: { pe: Draft }) {
  const { mutate } = useSWRConfig()
  const [editing, setEditing] = useState(false)
  const { reason } = reasonOf(pe)
  if (reason === 'budget_spent' && pe.status === PlatformExperimentStatus.OPEN) {
    return (
      <>
        <Button size="sm" onClick={() => setEditing(true)}>Edit budget</Button>
        {editing && <PlatformExperimentDialog initial={pe} onClose={() => setEditing(false)} onSaved={() => void mutate(['pe', pe.id])} />}
      </>
    )
  }
  if (reason === 'no_researchers') return <a className={textLinkClass} href={`#${AGENTS_CARD}`}>Add a researcher</a>
  if (GITHUB_REASONS.has(reason)) return <TextLink href={`/settings#${GITHUB_CARD}`}>Reconnect GitHub in Settings</TextLink>
  return null
}

const NEXT_IS: Record<Exclude<StatusReason['actor'], 'owner'>, string> = {
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
  const { actor } = reasonOf(pe)
  const why = (
    <p role="status" className="text-sm">
      {phase === 'baseline' && !pe.baseline_job_id && 'Preparing the baseline. No researcher starts and none of their machines is rented until it is measured. '}
      {phase === 'closed' && pe.closed_at && `Closed ${formatDate(pe.closed_at)}. `}
      {sentenceOf(pe)}
    </p>
  )
  const { data: agents } = useRoster(pe)
  const planner = agents?.find(agent => agent.flavor === 'planner')
  const baseline = baselineAgent(agents)
  return (
    <Card role="region" aria-label="Where this experiment stands">
      <CardContent className="grid gap-2.5">
        <Steps label="Phase" steps={PHASES} current={phase} live={phase !== 'closed'} />
        {readOnly || phase === 'closed' ? why : actor === 'owner' ? (
          <Callout tone="warning" className="mb-0 grid justify-items-start gap-1.5">
            <Eyebrow tone="warning">Your move</Eyebrow>
            {why}
            <OwnerAction pe={pe} />
          </Callout>
        ) : (
          <div>
            {why}
            <p className="text-xs text-muted">Nothing is needed from you: {NEXT_IS[actor]}.</p>
          </div>
        )}
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
