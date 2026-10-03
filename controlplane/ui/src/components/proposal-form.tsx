'use client'

import { useState } from 'react'
import type { ReactNode } from 'react'
import { ApiError } from '@/lib/api'
import { useAutoAccept } from '@/lib/auto-accept'
import { acceleratorLabel, formatAccH } from '@/lib/format'
import { acceptDraft } from '@/lib/planning'
import type { Draft, Proposal, ProposedAgent } from '@/lib/planning'
import { budgetUnit, kindOf } from '@/lib/units'
import type { MetricDefinition } from '@/types'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { FormField } from '@/components/ui/form-field'
import { Input } from '@/components/ui/input'
import { Callout, InlineError } from '@/components/ui/status-message'
import { Eyebrow, Pill } from '@/components/ui/text'

function metricLine(metric: MetricDefinition): string {
  if (metric.role === 'constraint') {
    return `${metric.key} must stay ${metric.direction === 'minimize' ? 'at or below' : 'at or above'} ${metric.bound}`
  }
  return `${metric.key}, ${metric.direction === 'minimize' ? 'lower is better' : 'higher is better'}`
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

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div>
      <Eyebrow className="mb-1 block">{title}</Eyebrow>
      {children}
    </div>
  )
}

/** What one node of the run's jobs asks for, of what the planner stated. */
function runsOn(proposal: Proposal): [string, string][] {
  const stated: [string, string | undefined][] = [
    [kindOf(proposal.accelerator_type) === 'cpu' ? 'Runs on' : 'Accelerator', acceleratorLabel(proposal.accelerator_type)],
    ['Image', proposal.image],
    ['CPU', proposal.cpu],
    ['Memory', proposal.memory],
    ['Storage', proposal.storage],
  ]
  return stated.filter((entry): entry is [string, string] => !!entry[1])
}

function Researcher({ n, agent }: { n: number; agent: ProposedAgent }) {
  const direction = agent.hyperparameters?.direction
  return (
    <li className="rounded border bg-surface-inset px-3 py-2">
      <div className="flex flex-wrap items-baseline gap-x-2.5 text-xs">
        <span className="font-medium">Researcher {n}</span>
        <span className="text-muted">specialization <span className="font-mono text-foreground">{agent.flavor ?? 'generalist'}</span></span>
        <span className="text-muted">model <span className="font-mono text-foreground">{agent.model ?? 'the platform\'s default'}</span></span>
      </div>
      <p className="mt-1 whitespace-pre-wrap text-sm">{direction ?? 'No direction of its own: it works from the brief.'}</p>
    </li>
  )
}

/**
 * What the planner proposed, as the experiment filled in: what is measured, how and on what,
 * who runs it, and what it may spend. The name, the budget, the duration and the number of
 * researchers are the person's to change, and accepting sends them as shown. Nothing has run.
 * `editable` is off for a reader and while the planner revises, when it is only read. A proposal
 * that `arrived` while the person watched it being planned is accepted by itself after a
 * countdown, unless they do anything in the form or in `children`, the box that asks the planner
 * for a change.
 */
export function ProposalForm({ draft, proposal, editable, arrived, onDone, children }: {
  draft: Draft
  proposal: Proposal
  editable: boolean
  arrived: boolean
  onDone: () => void
  children?: ReactNode
}) {
  const [name, setName] = useState(proposal.experiment.name)
  const [budget, setBudget] = useState(String(proposal.budget_accelerator_hours))
  const [hours, setHours] = useState(String(proposal.duration_hours))
  const [researchers, setResearchers] = useState(String(proposal.agents.length))
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // What the API refused the name for, said beside the name.
  const [nameError, setNameError] = useState<string | null>(null)

  const { metrics, stages, report_interval_seconds: reportEvery } = proposal.experiment
  const unit = budgetUnit(kindOf(proposal.accelerator_type), 'long')
  const count = Number(researchers)
  const countValid = Number.isInteger(count) && count > 0
  const valid = name.trim() !== '' && Number(budget) > 0 && Number(hours) > 0 && countValid
  // The researchers proposed keep what the planner gave them; one added beyond them is a generalist.
  const agents = Array.from({ length: countValid ? count : proposal.agents.length }, (_, i) => proposal.agents[i] ?? {})

  async function accept() {
    setSending(true)
    setError(null)
    setNameError(null)
    try {
      await acceptDraft(draft.id, { name: name.trim(), budget_accelerator_hours: Number(budget), duration_hours: Number(hours), agents })
      onDone()
    } catch (e) {
      // Accepting changes nothing of the proposal's experiment but its name.
      if (e instanceof ApiError && e.reason === 'invalid_experiment') setNameError(e.message)
      else setError(e instanceof Error ? e.message : String(e))
      setSending(false)
    }
  }

  const auto = useAutoAccept(editable && arrived, accept)

  return (
    // Any sign of a person at work in here ends the countdown: they are deciding for themselves.
    <div onFocus={auto.stop} onKeyDown={auto.stop} onPointerDown={auto.stop} onPointerMove={auto.stop} onWheel={auto.stop}>
    <Card>
      <CardHeader><CardTitle>Proposed experiment</CardTitle></CardHeader>
      <CardContent className="grid gap-4">
        {auto.secondsLeft !== null && (
          <Callout tone="accent" className="mb-0 flex flex-wrap items-center justify-between gap-3">
            <span>
              <span role="timer" className="font-medium">Starting in {auto.secondsLeft} s</span> as proposed. Touch anything here to decide yourself.
            </span>
            <Button size="sm" onClick={auto.stop}>Wait, let me edit</Button>
          </Callout>
        )}
        <Sentences title="Before you start, the planner asks" items={proposal.questions} />
        <FormField label="Name" error={nameError}>
          <Input value={name} disabled={!editable} onChange={e => setName(e.target.value)} />
        </FormField>
        <Section title="What is measured">
          <ul className="mb-2 grid gap-1.5">
            {metrics.map(metric => (
              <li key={metric.key} className="flex flex-wrap items-baseline gap-2">
                <Pill tone={metric.role === 'constraint' ? 'warning' : 'success'} active={metric.role !== 'constraint' && metric.role !== 'attribute'}>
                  {metricLine(metric)}
                </Pill>
                {metric.description && <span className="text-xs text-muted">{metric.description}</span>}
              </li>
            ))}
          </ul>
          <p className="text-sm">{proposal.metric_reason}</p>
        </Section>
        <Section title="How it is measured">
          <p className="text-sm">{proposal.measurement}</p>
          <dl className="mt-2 flex flex-wrap gap-x-5 gap-y-1 text-xs">
            {runsOn(proposal).map(([label, value]) => (
              <div key={label} className="flex items-baseline gap-1.5">
                <dt className="text-muted">{label}</dt>
                <dd className="font-mono">{value}</dd>
              </div>
            ))}
          </dl>
          <p className="mt-1 text-xs text-muted">
            Nothing has run yet. Once you accept, the baseline is measured on {acceleratorLabel(proposal.accelerator_type)} before any researcher starts.
          </p>
        </Section>
        <Section title="Researchers">
          <ol className="grid gap-1.5">
            {agents.map((agent, i) => <Researcher key={i} n={i + 1} agent={agent} />)}
          </ol>
        </Section>
        {(!!stages?.length || !!reportEvery) && (
          <Section title="Rules of the run">
            <ul className="list-disc pl-5 text-sm">
              {stages?.map((stage, i) => (
                <li key={i}>
                  Stage {i + 1}: {stage.length_pct}% of the run
                  {stage.evict_pct > 0 ? `, then the worst ${stage.evict_pct}% of researchers are cut` : ', nobody is cut'}
                  {stage.max_job_hours ? `, no job longer than ${stage.max_job_hours} h` : ''}
                </li>
              ))}
              {!!reportEvery && <li>Every job reports its metrics every {reportEvery} s</li>}
            </ul>
          </Section>
        )}
        {proposal.sizing_reason && (
          <Section title="Why this size">
            <p className="text-sm">{proposal.sizing_reason}</p>
          </Section>
        )}
        <Section title="What it may spend">
          <p className="mb-2 text-sm text-muted">
            These three are yours to state. The planner proposed {formatAccH(proposal.budget_accelerator_hours)} {unit} over {proposal.duration_hours} hours
            for {proposal.agents.length} {proposal.agents.length === 1 ? 'researcher' : 'researchers'}; the figures are an estimate, since nothing has run, and the budget is a cap on everything the run spends, the baseline and burst hours included.
          </p>
          <div className="grid gap-3 sm:grid-cols-3">
            <FormField label={`Budget (${unit})`}><Input type="number" min={0} step="any" value={budget} disabled={!editable} onChange={e => setBudget(e.target.value)} /></FormField>
            <FormField label="Duration (hours)"><Input type="number" min={0} step="any" value={hours} disabled={!editable} onChange={e => setHours(e.target.value)} /></FormField>
            <FormField label="Researchers"><Input type="number" min={1} step={1} value={researchers} disabled={!editable} onChange={e => setResearchers(e.target.value)} /></FormField>
          </div>
        </Section>
        <Sentences title="What could mislead" items={proposal.risks} />
        <Sentences title="Left out on purpose" items={proposal.out_of_scope} />
        <Section title="The brief every researcher works from">
          <CollapsibleDescription text={proposal.experiment.description} />
        </Section>
        {editable && (
          <div className="flex items-center gap-3">
            <Button variant="primary" loading={sending} disabled={!valid} onClick={accept}>Accept and run</Button>
            {error && <InlineError>{error}</InlineError>}
          </div>
        )}
      </CardContent>
    </Card>
    {children}
    </div>
  )
}
