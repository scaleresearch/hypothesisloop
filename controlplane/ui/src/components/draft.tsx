'use client'

import { useState } from 'react'
import useSWR, { useSWRConfig } from 'swr'
import { fetchExperimentMetrics, fetchPlatformExperimentAgents } from '@/lib/api'
import { formatAccH, formatDate, relTime } from '@/lib/format'
import { abandonDraft, acceptAndStart, draftState, reviseDraft } from '@/lib/planning'
import type { Draft, Proposal } from '@/lib/planning'
import { budgetUnit, kindOf } from '@/lib/units'
import { useProjectRouter } from '@/lib/query-state'
import type { MetricDataPoint, MetricDefinition } from '@/types'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader } from '@/components/ui/card'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { FormField } from '@/components/ui/form-field'
import { Input, Textarea } from '@/components/ui/input'
import { Link } from '@/components/ui/link'
import { PageHeader } from '@/components/ui/page-header'
import { Callout, InlineError } from '@/components/ui/status-message'
import { Eyebrow, Pill } from '@/components/ui/text'

const said = (error: unknown) => (error instanceof Error ? error.message : String(error))

/** The last raw value a job reported for each metric key. */
export function lastReported(points: MetricDataPoint[]): Map<string, number> {
  const last = new Map<string, number>()
  for (const point of points) {
    if (!point.metric_basis || point.metric_basis === 'raw') last.set(point.metric_name, point.metric_value)
  }
  return last
}

function metricLine(metric: MetricDefinition, baseline: Map<string, number> | undefined): string {
  const value = baseline?.get(metric.key)
  const measured = value === undefined ? '' : `, baseline ${value}`
  if (metric.role === 'constraint') {
    return `${metric.key} must stay ${metric.direction === 'minimize' ? 'at or below' : 'at or above'} ${metric.bound}${measured}`
  }
  return `${metric.key}, ${metric.direction === 'minimize' ? 'lower is better' : 'higher is better'}${measured}`
}

function Sentences({ title, items }: { title: string; items?: string[] }) {
  if (!items || items.length === 0) return null
  return (
    <div>
      <Eyebrow className="mb-1 block">{title}</Eyebrow>
      <ul className="list-disc pl-5 text-sm">
        {items.map(item => <li key={item}>{item}</li>)}
      </ul>
    </div>
  )
}

/** What the planner proposed, in the order a person decides by: what is measured, on what evidence, at what cost. */
function ProposalCard({ proposal }: { proposal: Proposal }) {
  // Read from the job itself: the proposal carries no number of its own to trust.
  const { data: points } = useSWR(['draft-baseline', proposal.baseline_job_id], () => fetchExperimentMetrics(proposal.baseline_job_id))
  const baseline = points && lastReported(points)
  const metrics = proposal.experiment.metrics ?? []
  return (
    <Card>
      <CardHeader>Proposed experiment: {proposal.experiment.name}</CardHeader>
      <CardContent className="grid gap-4">
        <Sentences title="Before you start, the planner asks" items={proposal.questions} />
        <div>
          <Eyebrow className="mb-1 block">What is measured</Eyebrow>
          <div className="mb-2 flex flex-wrap gap-2">
            {metrics.map(metric => (
              <Pill key={metric.key} tone={metric.role === 'constraint' ? 'warning' : 'success'} active={metric.role !== 'constraint' && metric.role !== 'attribute'}>
                {metricLine(metric, baseline)}
              </Pill>
            ))}
          </div>
          <p className="text-sm">{proposal.metric_reason}</p>
        </div>
        <div>
          <Eyebrow className="mb-1 block">How it is measured</Eyebrow>
          <p className="text-sm">{proposal.measurement}</p>
          <p className="mt-1 text-xs text-muted">
            The baseline was measured by <Link href={`/jobs/${proposal.baseline_job_id}`} className="text-accent hover:underline">job {proposal.baseline_job_id}</Link>
            {` on ${proposal.accelerator_type}.`}
          </p>
        </div>
        <Sentences title="What could mislead" items={proposal.risks} />
        <Sentences title="Left out on purpose" items={proposal.out_of_scope} />
        <div>
          <Eyebrow className="mb-1 block">The brief every researcher works from</Eyebrow>
          <CollapsibleDescription text={proposal.experiment.description} />
        </div>
      </CardContent>
    </Card>
  )
}

/** What the run may spend and who runs it, stated by the person: the proposal's figures are where the fields start. */
function Accept({ draft, proposal, onDone }: { draft: Draft; proposal: Proposal; onDone: () => void }) {
  const [budget, setBudget] = useState(String(proposal.budget_accelerator_hours))
  const [hours, setHours] = useState(String(proposal.duration_hours))
  const [researchers, setResearchers] = useState(String(proposal.agents.length))
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const unit = budgetUnit(kindOf(proposal.accelerator_type), 'long')
  const count = Number(researchers)
  const valid = Number(budget) > 0 && Number(hours) > 0 && Number.isInteger(count) && count > 0

  async function accept() {
    setSending(true)
    setError(null)
    // The researchers proposed keep their flavors; one added beyond them is a generalist.
    const agents = Array.from({ length: count }, (_, i) => proposal.agents[i] ?? {})
    try {
      // A start the API refused is left to the platform, which starts an accepted draft itself.
      await acceptAndStart(draft.id, { budget_accelerator_hours: Number(budget), duration_hours: Number(hours), agents })
      onDone()
    } catch (e) {
      setError(said(e))
      setSending(false)
    }
  }

  return (
    <Card>
      <CardHeader>Accept and run</CardHeader>
      <CardContent className="grid gap-3">
        <p className="text-sm text-muted">
          These three are yours to state. The planner proposed {formatAccH(proposal.budget_accelerator_hours)} {unit} over {proposal.duration_hours} hours
          for {proposal.agents.length} {proposal.agents.length === 1 ? 'researcher' : 'researchers'}; the run spends nothing beyond what you enter here.
        </p>
        <div className="grid gap-3 sm:grid-cols-3">
          <FormField label={`Budget (${unit})`}><Input type="number" min={0} step="any" value={budget} onChange={e => setBudget(e.target.value)} /></FormField>
          <FormField label="Duration (hours)"><Input type="number" min={0} step="any" value={hours} onChange={e => setHours(e.target.value)} /></FormField>
          <FormField label="Researchers"><Input type="number" min={1} step={1} value={researchers} onChange={e => setResearchers(e.target.value)} /></FormField>
        </div>
        <div className="flex items-center gap-3">
          <Button variant="primary" loading={sending} disabled={!valid} onClick={accept}>Accept and run</Button>
          {error && <InlineError>{error}</InlineError>}
        </div>
      </CardContent>
    </Card>
  )
}

function Revise({ draft, declined, onDone }: { draft: Draft; declined: boolean; onDone: () => void }) {
  const [message, setMessage] = useState('')
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function revise() {
    setSending(true)
    setError(null)
    try {
      await reviseDraft(draft.id, message.trim())
      setMessage('')
      onDone()
    } catch (e) {
      setError(said(e))
    }
    setSending(false)
  }

  return (
    <Card>
      <CardHeader>{declined ? 'Answer the planner' : 'Ask for a change'}</CardHeader>
      <CardContent className="grid gap-2">
        <FormField
          label={declined ? 'What was missing?' : 'What should be different?'}
          hint={declined
            ? 'The planner starts again with what it knows and your answer.'
            : 'The planner starts again with what it knows and proposes anew. The proposal above stays until it does.'}
        >
          <Textarea
            rows={2}
            value={message}
            onChange={e => setMessage(e.target.value)}
            placeholder={declined ? 'e.g. The small transformer in train.py, judged by validation loss.' : 'e.g. It must fit a single RTX 4090. Measure latency too.'}
          />
        </FormField>
        <div className="flex items-center gap-3">
          <Button loading={sending} disabled={message.trim() === ''} onClick={revise}>Revise</Button>
          {error && <InlineError>{error}</InlineError>}
        </div>
      </CardContent>
    </Card>
  )
}

/** What the planner's machine is doing, while there is no proposal to read. */
function Planning({ draft, revising }: { draft: Draft; revising: boolean }) {
  const { data: agents } = useSWR(['pe-agents', draft.id], () => fetchPlatformExperimentAgents(draft.id), { refreshInterval: 10_000 })
  const planner = agents?.find(a => a.flavor === 'planner')?.instance
  const machine = !planner ? 'Its machine is being asked for.'
    : planner.stopped ? 'Its machine stopped and is not asked for again. Abandon this draft and try once more.'
      : planner.state === 'running' ? `Its machine is running${planner.progress_at ? `, last progress ${relTime(planner.progress_at)}` : ''}.`
        : 'Its machine is starting, which takes about a minute.'
  return (
    <Callout tone="info">
      <span role="status">
        {revising ? 'The planner is changing its proposal.' : 'The planner is working out how to measure this and running a baseline.'} {machine}{' '}
        It has until {formatDate(draft.ends_at)}; a draft with no proposal by then is closed. This page updates by itself.
      </span>
    </Callout>
  )
}

/**
 * The page of a platform experiment that is still a draft: the sentence it came from, what its
 * planner is doing or proposed, and the two things a person does with a proposal, accepting it
 * at a cost they state or asking for a change. A reader of a shared draft is shown neither.
 */
export function DraftView({ draft, readOnly = false }: { draft: Draft; readOnly?: boolean }) {
  const router = useProjectRouter()
  const { mutate } = useSWRConfig()
  const [abandonError, setAbandonError] = useState<string | null>(null)
  const state = draftState(draft)
  const reread = () => void mutate(['pe', draft.id])

  async function abandon() {
    setAbandonError(null)
    try {
      await abandonDraft(draft.id)
      reread()
    } catch (e) {
      setAbandonError(said(e))
    }
  }

  return (
    <div>
      <PageHeader
        back={{ label: 'Experiments', onClick: () => router.push('/platform-experiments') }}
        title={draft.name}
        description={<span className="text-xs text-subtle">Draft, {state === 'proposed' || state === 'declined' ? 'waiting for you' : 'being planned'}</span>}
      />
      <Card>
        <CardHeader>What you asked</CardHeader>
        <CardContent className="grid gap-2">
          <p className="text-sm">{draft.goal}</p>
          {(draft.revisions ?? []).map(revision => (
            <p key={revision.at} className="text-sm">
              <span className="text-xs text-muted">{revision.by.name || revision.by.kind} asked {relTime(revision.at)}: </span>
              {revision.message}
            </p>
          ))}
        </CardContent>
      </Card>
      {(state === 'planning' || state === 'revising') && <Planning draft={draft} revising={state === 'revising'} />}
      {draft.declined_reason && (
        <Card>
          <CardHeader>The planner proposed no experiment</CardHeader>
          <CardContent className="grid gap-2">
            <p className="text-sm">{draft.declined_reason}</p>
            {state === 'declined' && (
              <p className="text-xs text-muted">
                Nothing runs and nothing is spent. Answer below to have it try again, or abandon this draft; it is closed on {formatDate(draft.ends_at)} otherwise.
              </p>
            )}
          </CardContent>
        </Card>
      )}
      {draft.proposal && <ProposalCard proposal={draft.proposal} />}
      {!readOnly && state === 'proposed' && draft.proposal && <Accept draft={draft} proposal={draft.proposal} onDone={reread} />}
      {!readOnly && (state === 'proposed' || state === 'declined') && <Revise draft={draft} declined={state === 'declined'} onDone={reread} />}
      {!readOnly && (
        <div className="mt-2 flex items-center gap-3">
          <Button variant="ghost" onClick={abandon}>Abandon this draft</Button>
          {abandonError && <InlineError>{abandonError}</InlineError>}
        </div>
      )}
    </div>
  )
}
