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
import { Captioned } from '@/components/ui/captioned'
import { InsetCard } from '@/components/ui/card'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { Facts } from '@/components/ui/facts'
import { FormField } from '@/components/ui/form-field'
import { Input } from '@/components/ui/input'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Callout, InlineError } from '@/components/ui/status-message'
import { Code, Pill, Text } from '@/components/ui/text'

function metricLine(metric: MetricDefinition): string {
  if (metric.role === 'constraint') {
    return `${metric.key} must stay ${metric.direction === 'minimize' ? 'at or below' : 'at or above'} ${metric.bound}`
  }
  return `${metric.key}, ${metric.direction === 'minimize' ? 'lower is better' : 'higher is better'}`
}

const BULLETS = 'list-disc pl-5 text-sm'

/** A captioned list of the planner's sentences; nothing when it said none. */
function Sentences({ title, items }: { title: string; items?: string[] }) {
  if (!items || items.length === 0) return null
  return (
    <Captioned caption={title}>
      <ul className={BULLETS}>
        {items.map(item => <li key={item}>{item}</li>)}
      </ul>
    </Captioned>
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
    <InsetCard as="li" gap="xs">
      <Inline gap="md" align="baseline" className="text-xs">
        <span className="font-medium">Researcher {n}</span>
        <span className="text-muted">specialization <Code className="text-foreground">{agent.flavor ?? 'generalist'}</Code></span>
        <span className="text-muted">model <Code className="text-foreground">{agent.model ?? 'the platform\'s default'}</Code></span>
      </Inline>
      <Text pre>{direction ?? 'No direction of its own: it works from the brief.'}</Text>
    </InsetCard>
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
    <Stack gap="xl">
    <Section title="Proposed experiment">
      <Stack gap="lg">
        {auto.secondsLeft !== null && (
          <Callout tone="accent" className="flex flex-wrap items-center justify-between gap-3">
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
        <Captioned caption="What is measured">
          <Stack as="ul" gap="xs" className="mb-2">
            {metrics.map(metric => (
              <Inline as="li" align="baseline" key={metric.key}>
                <Pill tone={metric.role === 'constraint' ? 'warning' : 'success'} active={metric.role !== 'constraint' && metric.role !== 'attribute'}>
                  {metricLine(metric)}
                </Pill>
                {metric.description && <Text as="span" variant="note">{metric.description}</Text>}
              </Inline>
            ))}
          </Stack>
          <Text>{proposal.metric_reason}</Text>
        </Captioned>
        <Captioned caption="How it is measured">
          <Stack gap="sm">
            <Text>{proposal.measurement}</Text>
            <Facts facts={runsOn(proposal)} />
            <Text variant="note">
              Nothing has run yet. Once you accept, the baseline is measured on {acceleratorLabel(proposal.accelerator_type)} before any researcher starts.
            </Text>
          </Stack>
        </Captioned>
        <Captioned caption="Researchers">
          <ol className="grid gap-1.5">
            {agents.map((agent, i) => <Researcher key={i} n={i + 1} agent={agent} />)}
          </ol>
        </Captioned>
        {(!!stages?.length || !!reportEvery) && (
          <Captioned caption="Rules of the run">
            <ul className={BULLETS}>
              {stages?.map((stage, i) => (
                <li key={i}>
                  Stage {i + 1}: {stage.length_pct}% of the run
                  {stage.evict_pct > 0 ? `, then the worst ${stage.evict_pct}% of researchers are cut` : ', nobody is cut'}
                  {stage.max_job_hours ? `, no job longer than ${stage.max_job_hours} h` : ''}
                </li>
              ))}
              {!!reportEvery && <li>Every job reports its metrics every {reportEvery} s</li>}
            </ul>
          </Captioned>
        )}
        {proposal.sizing_reason && (
          <Captioned caption="Why this size">
            <Text>{proposal.sizing_reason}</Text>
          </Captioned>
        )}
        <Captioned caption="What it may spend">
          <Text tone="muted" className="mb-2">
            These three are yours to state. The planner proposed {formatAccH(proposal.budget_accelerator_hours)} {unit} over {proposal.duration_hours} hours
            for {proposal.agents.length} {proposal.agents.length === 1 ? 'researcher' : 'researchers'}; the figures are an estimate, since nothing has run, and the budget is a cap on everything the run spends, the baseline and burst hours included.
          </Text>
          <div className="grid gap-3 sm:grid-cols-3">
            <FormField label={`Budget (${unit})`}><Input type="number" min={0} step="any" value={budget} disabled={!editable} onChange={e => setBudget(e.target.value)} /></FormField>
            <FormField label="Duration (hours)"><Input type="number" min={0} step="any" value={hours} disabled={!editable} onChange={e => setHours(e.target.value)} /></FormField>
            <FormField label="Researchers"><Input type="number" min={1} step={1} value={researchers} disabled={!editable} onChange={e => setResearchers(e.target.value)} /></FormField>
          </div>
        </Captioned>
        <Sentences title="What could mislead" items={proposal.risks} />
        <Sentences title="Left out on purpose" items={proposal.out_of_scope} />
        <Captioned caption="The brief every researcher works from">
          <CollapsibleDescription text={proposal.experiment.description} />
        </Captioned>
        {editable && (
          <Inline gap="md">
            <Button variant="primary" loading={sending} disabled={!valid} onClick={accept}>Accept and run</Button>
            {error && <InlineError>{error}</InlineError>}
          </Inline>
        )}
      </Stack>
    </Section>
    {children}
    </Stack>
    </div>
  )
}
