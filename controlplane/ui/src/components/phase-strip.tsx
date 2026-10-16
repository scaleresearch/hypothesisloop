'use client'

import { useState } from 'react'
import type { ReactNode } from 'react'
import { useSWRConfig } from 'swr'
import type { StagesStatus } from '@/lib/api'
import { formatDate } from '@/lib/format'
import { OWNER_ACTIONS, PHASES, phaseOf, reasonOf, waitsFor } from '@/lib/phase'
import type { Draft } from '@/lib/planning'
import { useRoster } from '@/lib/roster'
import { ExperimentStatus } from '@/types'
import type { Experiment, JoinedAgent, StatusReason } from '@/types'
import { actorLabel } from '@/components/actor'
import { AgentAtWork, PlannerAtWork } from '@/components/agent-at-work'
import { AGENTS_CARD } from '@/components/experiment-agents'
import { JobReason } from '@/components/job-reason'
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { ProposalForm } from '@/components/proposal-form'
import { RunProgress } from '@/components/run-progress'
import { Button } from '@/components/ui/button'
import { Card, CardContent, InsetCard } from '@/components/ui/card'
import { TextLink, textLinkClass } from '@/components/ui/link'
import { Callout } from '@/components/ui/status-message'
import { Steps } from '@/components/ui/steps'
import { Eyebrow, Text } from '@/components/ui/text'
import { jobPath } from '@/lib/routes'

// The states this page has words of its own for, by status_reason.reason. Any other is said in
// the API's message, which is worded for whoever reads it.
const SENTENCE: Record<string, (pe: Draft, baselineName?: string) => string> = {
  repository_creating: () => 'Creating your experiment\'s repository.',
  planning: () => 'Planning your experiment.',
  revising: () => 'Updating the plan with your change.',
  accepting: () => 'Your plan is ready and starts by itself in a moment, as proposed. Accept it now, edit it or reject it below.',
  proposed: pe => `Your plan is ready. Nothing runs or is spent until you accept it. It stays open until ${formatDate(pe.proposal_expires_at)}.`,
  declined: () => 'The planner needs more from you. Its reason is below.',
  baseline_measuring: (_, name) => `${name ?? 'The baseline agent'} is measuring the baseline. The run starts by itself when it finishes.`,
  baseline_declaring: () => 'The baseline is measured and the platform is declaring it. The run starts next.',
  starting: () => 'Everything is ready and the run starts within a minute.',
  agents_starting: pe => `The researchers' machines are starting. The platform closes the experiment if none is running by ${formatDate(pe.agents_due_at)}.`,
  running: pe => `The researchers are at work until ${formatDate(pe.ends_at)}.`,
  researchers_done: pe => `No researcher is left to submit a job: each one finished, was cut or lost its machine. Its coordinator adds researchers in their place while time and quota are left with \`hl platform-experiments agents add --hosted --platform-experiment ${pe.id}\`. The platform closes the experiment on ${formatDate(pe.ends_at)}. Before then its summary is written with \`hl platform-experiments summary --file FILE --platform-experiment ${pe.id}\` and it is closed with \`hl platform-experiments close --platform-experiment ${pe.id}\`.`,
  end_time_reached: () => 'It reached its end time.',
  closed_by_request: pe => `It was closed early by ${actorLabel(pe.closed_by!)}${pe.close_message ? `, who said: ${pe.close_message}` : '.'}`,
  planning_limit_reached: () => 'It never ran: its planner proposed nothing within the planning limit.',
  proposal_expired: () => 'It never ran: nobody took up its planner\'s answer in time.',
  baseline_limit_reached: () => 'It never ran: no baseline was measured in time.',
  planner_stopped: () => 'It never ran: its planner\'s machine ended before it answered, as often as the platform restarts one. Nothing was spent on a run, and the same sentence makes a new draft.',
  agents_not_started: () => 'It started, and no researcher\'s machine was running in time, so the platform closed it. Agents, below, says why each machine did not come up.',
}

/** The API's message for a reason this page has no words for: capitalised, with no experiment id in it. */
export function humanize(message: string): string {
  const plain = message.replace(/\s+of platform experiment pe-[\w-]+/g, '').replace(/platform experiment pe-[\w-]+/g, 'this experiment').trim()
  return plain.charAt(0).toUpperCase() + plain.slice(1)
}

export function sentenceOf(pe: Draft, baselineName?: string): string {
  const { reason, message } = reasonOf(pe)
  return SENTENCE[reason]?.(pe, baselineName) ?? humanize(message)
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
  if (!job) return <Text variant="note">It has submitted no job yet.</Text>
  const link = <TextLink inline href={jobPath(job.platform_experiment_id, job.id)}>Its job</TextLink>
  const value = job.metric ? `: ${job.metric.name} ${job.metric.value}` : ''
  switch (job.status) {
    case ExperimentStatus.SUBMITTED:
    case ExperimentStatus.QUEUED: {
      const why = job.not_admitted_message ?? job.not_admitted_reason
      return <Text variant="note" tone="default">{link} is queued{why ? `: ${why}` : '.'}</Text>
    }
    case ExperimentStatus.RUNNING:
      return <Text variant="note" tone="default">{link} is running{value}.</Text>
    case ExperimentStatus.COMPLETED:
      return <Text variant="note" tone="default">{link} is done{value}. The platform declares it the baseline and starts the run once the agent finishes.</Text>
    default:
      return (
        <Text as="div" variant="note" tone="default">
          {link} ended {job.status.toLowerCase()}.
          {job.eviction_reason && <JobReason reason={job.eviction_reason} />}
        </Text>
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
 * looked for. `stages` puts how far the run is under the phases while it runs, and `children`
 * say what its machines are doing. A reader of a shared experiment is told neither that a move is
 * theirs nor that none is. Past its proposal, the Proposal step opens the proposal as it was
 * accepted, to read.
 */
export function PhaseStrip({ pe, jobs = [], stages, readOnly = false, children }: {
  pe: Draft
  jobs?: Experiment[]
  stages?: StagesStatus
  readOnly?: boolean
  children?: ReactNode
}) {
  const phase = phaseOf(pe)
  const { data: agents } = useRoster(pe)
  const why = (
    <Text role="status">
      {phase === 'baseline' && !pe.baseline_job_id && reasonOf(pe).reason !== 'repository_creating' && 'Preparing the baseline. No researcher starts and none of their machines is rented until it is measured. '}
      {phase === 'closed' && pe.closed_at && `Closed ${formatDate(pe.closed_at)}. `}
      {sentenceOf(pe, baselineAgent(agents)?.name)}
    </Text>
  )
  // A closed experiment waits for nobody, and a reader is told neither whose move it is.
  const who = readOnly || phase === 'closed' ? undefined : waitsFor(pe)
  const planner = agents?.find(agent => agent.flavor === 'planner')
  const baseline = baselineAgent(agents)
  const [showProposal, setShowProposal] = useState(false)
  const pastProposal = phase !== 'planning' && phase !== 'proposal' && !!pe.proposal
  return (
    <>
    <Card role="region" aria-label="Where this experiment stands">
      <CardContent className="grid gap-3">
        <Steps
          label="Phase" steps={PHASES} current={phase} live={phase !== 'closed'}
          open={pastProposal ? ['proposal'] : []} shown={showProposal ? 'proposal' : undefined} onOpen={() => setShowProposal(shown => !shown)}
        />
        {phase === 'running' && pe.started_at && stages && stages.stages.length > 0 && <RunProgress status={stages} />}
        {!who ? why : who === 'owner' ? (
          <Callout tone="warning" className="grid justify-items-start gap-1.5">
            <Eyebrow tone="warning">Your move</Eyebrow>
            {why}
            <OwnerMove pe={pe} />
          </Callout>
        ) : (
          <div>
            {why}
            {phase !== 'planning' && <Text variant="note">Nothing is needed from you: {NEXT_IS[who]}.</Text>}
          </div>
        )}
        {children}
        {phase === 'planning' && <PlannerAtWork agent={planner} activity={pe.activity} />}
        {phase === 'baseline' && baseline && (
          <InsetCard role="group" aria-label="Baseline agent">
            <Eyebrow>Baseline agent {baseline.name}</Eyebrow>
            <AgentAtWork agent={baseline} />
            <BaselineJob job={jobs.find(job => job.agent_id === baseline.agent_id)} />
          </InsetCard>
        )}
      </CardContent>
    </Card>
    {pastProposal && showProposal && (
      <ProposalForm key={pe.proposed_at} draft={pe} proposal={pe.proposal!} editable={false} accepting={false} accepted onDone={() => {}} />
    )}
    </>
  )
}
