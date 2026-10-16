'use client'

import { useState } from 'react'
import type { ReactNode } from 'react'
import { ApiError } from '@/lib/api'
import { useSecondsUntil } from '@/lib/auto-accept'
import { acceleratorLabel, formatAccH, formatDate } from '@/lib/format'
import { abandonDraft, acceptDraft, holdDraft } from '@/lib/planning'
import type { Draft, Proposal, ProposedAgent } from '@/lib/planning'
import { budgetUnit, kindOf } from '@/lib/units'
import type { MetricDefinition } from '@/types'
import { actorLabel } from '@/components/actor'
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
import { errorMessage } from '@/lib/errors'

type Stated = 'name' | 'budget' | 'baseline' | 'hours' | 'researchers'

// The figure an accept's refusal concerns, by the API's reason. Accepting changes nothing of the
// proposal's experiment but its name, so invalid_experiment is the name's.
const FIELD_OF: Record<string, Stated> = {
  invalid_experiment: 'name',
  invalid_cost: 'budget',
  invalid_baseline_minutes: 'baseline',
  invalid_length: 'hours',
  run_too_long: 'hours',
  invalid_agents: 'researchers',
}

function metricLine(metric: MetricDefinition): string {
  if (metric.role === 'constraint') {
    return `${metric.key} must stay ${metric.direction === 'minimize' ? 'at or below' : 'at or above'} ${metric.bound}`
  }
  const line = `${metric.key}, ${metric.direction === 'minimize' ? 'lower is better' : 'higher is better'}`
  if (metric.target_vs_baseline === undefined) return line
  const pct = Math.round(metric.target_vs_baseline * 1000) / 10
  return `${line}, target ${pct > 0 ? '+' : ''}${pct}% against the baseline`
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

/** The countdown to the platform accepting the proposal by itself, with the three ways to act first. */
function Countdown({ draft, accept, sending, onDone }: { draft: Draft; accept: () => void; sending: boolean; onDone: () => void }) {
  const secondsLeft = useSecondsUntil(draft.accepts_at)
  const [acting, setActing] = useState<'edit' | 'reject' | null>(null)
  const [error, setError] = useState<string | null>(null)

  async function act(which: 'edit' | 'reject') {
    setActing(which)
    setError(null)
    try {
      await (which === 'edit' ? holdDraft(draft.id) : abandonDraft(draft.id))
      onDone()
    } catch (e) {
      setError(errorMessage(e))
      setActing(null)
    }
  }

  return (
    <Callout tone="accent" className="flex flex-wrap items-center justify-between gap-3">
      <span>
        <span role="timer" className="font-medium">{secondsLeft ? `Starting in ${secondsLeft} s` : 'Starting now'}</span> as proposed. The baseline machine is already starting.
      </span>
      <Inline gap="sm">
        <Button size="sm" variant="primary" loading={sending} disabled={!!acting} onClick={accept}>Accept now</Button>
        <Button size="sm" loading={acting === 'edit'} disabled={sending || !!acting} onClick={() => act('edit')}>Edit</Button>
        <Button size="sm" variant="ghost" loading={acting === 'reject'} disabled={sending || !!acting} onClick={() => act('reject')}>Reject</Button>
        {error && <InlineError>{error}</InlineError>}
      </Inline>
    </Callout>
  )
}

/**
 * What the planner proposed, as the experiment filled in: what is measured, how and on what,
 * who runs it, and what it may spend. The name, the budget, the duration, the minutes the baseline
 * has, the number of researchers and the answers to the planner's questions are the person's to
 * change, and accepting sends them as shown.
 * Nothing has run.
 * `editable` is off for a reader, while the planner revises and while the platform counts down
 * to accepting it by itself, when it is only read. `accepting` shows that countdown, with Accept
 * now, Edit, which stops the countdown and makes the form editable, and Reject, which closes the
 * draft. `children` is the box that asks the planner for a change. `accepted` shows a proposal
 * that was accepted as the record of it: the same layout with the values as text and no control.
 */
export function ProposalForm({ draft, proposal, editable, accepting, accepted = false, onDone, children }: {
  draft: Draft
  proposal: Proposal
  editable: boolean
  accepting: boolean
  accepted?: boolean
  onDone: () => void
  children?: ReactNode
}) {
  const [name, setName] = useState(proposal.experiment.name)
  const [budget, setBudget] = useState(String(proposal.budget_accelerator_hours))
  const [hours, setHours] = useState(String(proposal.duration_hours))
  const [baselineMinutes, setBaselineMinutes] = useState(String(proposal.baseline_minutes))
  const [researchers, setResearchers] = useState(String(proposal.agents.length))
  const questions = proposal.questions ?? []
  const [answers, setAnswers] = useState(() => questions.map(q => q.answer))
  const [sending, setSending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // A refusal of one of the figures the person states, said beside it.
  const [refusal, setRefusal] = useState<{ field: Stated; message: string } | null>(null)
  const errorOf = (field: Stated) => refusal?.field === field ? refusal.message : null

  const { metrics, stages, report_interval_seconds: reportEvery } = proposal.experiment
  const unit = budgetUnit(kindOf(proposal.accelerator_type), 'long')
  const count = Number(researchers)
  // The researchers proposed keep what the planner gave them; one added beyond them is a generalist.
  // A count that is no whole number sends none, which the API refuses with why.
  const agents = Array.from({ length: Number.isInteger(count) && count > 0 ? count : 0 }, (_, i) => proposal.agents[i] ?? {})

  async function accept() {
    setSending(true)
    setError(null)
    setRefusal(null)
    try {
      await acceptDraft(draft.id, {
        name: name.trim(), budget_accelerator_hours: Number(budget), duration_hours: Number(hours), baseline_minutes: Number(baselineMinutes), agents,
        ...(questions.length > 0 && { answers: answers.map(answer => answer.trim()) }),
      })
      onDone()
    } catch (e) {
      const field = e instanceof ApiError && e.reason ? FIELD_OF[e.reason] : undefined
      if (field) setRefusal({ field, message: (e as ApiError).message })
      else setError(errorMessage(e))
      setSending(false)
    }
  }

  return (
    <Stack gap="xl">
    <Section title={accepted ? 'Accepted proposal' : 'Proposed experiment'}>
      <Stack gap="lg">
        {accepted && draft.accepted_at && (
          <Text role="note">
            {draft.accepted_by ? `Accepted by ${actorLabel(draft.accepted_by)} on ${formatDate(draft.accepted_at)}.` : `Accepted automatically on ${formatDate(draft.accepted_at)}.`}
          </Text>
        )}
        {accepting && <Countdown draft={draft} accept={accept} sending={sending} onDone={onDone} />}
        {accepting && error && <InlineError>{error}</InlineError>}
        {accepted && questions.length > 0 && (
          <Captioned caption="The planner's questions and the answers given">
            <dl className="grid gap-3">
              {questions.map((q, i) => {
                const given = draft.accepted_answers?.[i]
                return (
                  <div key={i}>
                    <dt className="text-sm text-muted">{q.question}</dt>
                    <dd>
                      <Text>{given ?? q.answer}</Text>
                      {given === undefined
                        ? <Text variant="note">The answer the planner planned on.</Text>
                        : given !== q.answer && <Text variant="note">The planner had planned on: {q.answer}</Text>}
                    </dd>
                  </div>
                )
              })}
            </dl>
          </Captioned>
        )}
        {!accepted && questions.length > 0 && (
          <Captioned caption="Before you start, answer the planner's questions">
            <Stack gap="sm">
              <Text tone="muted">
                The goal left these open. Each answer is the one the planner planned on: keep it or change it. Your answers are added to the brief every agent reads, so this proposal waits for you.
              </Text>
              {questions.map((q, i) => (
                <FormField key={i} label={q.question}>
                  <Input value={answers[i]} disabled={!editable} onChange={e => setAnswers(answers.map((answer, j) => j === i ? e.target.value : answer))} />
                </FormField>
              ))}
            </Stack>
          </Captioned>
        )}
        {accepted ? (
          <Captioned caption="Name"><Text>{proposal.experiment.name}</Text></Captioned>
        ) : (
          <FormField label="Name" error={errorOf('name')}>
            <Input value={name} disabled={!editable} onChange={e => setName(e.target.value)} />
          </FormField>
        )}
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
            {!accepted && (
              <Text variant="note">
                Nothing has run yet. Once you accept, the baseline is measured on {acceleratorLabel(proposal.accelerator_type)} before any researcher starts.
              </Text>
            )}
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
        {accepted ? (
          <Captioned caption="What it may spend, as proposed">
            <Text tone="muted" className="mb-2">
              The planner estimated these before anything ran. Whoever accepted stated the figures the run started with, shown on this page.
            </Text>
            <Facts facts={[
              [`Budget (${unit})`, formatAccH(proposal.budget_accelerator_hours)],
              ['Baseline (minutes)', String(proposal.baseline_minutes)],
              ['Duration (hours)', String(proposal.duration_hours)],
              ['Researchers', String(proposal.agents.length)],
            ]} />
          </Captioned>
        ) : (
        <Captioned caption="What it may spend">
          <Text tone="muted" className="mb-2">
            These four are yours to state. The planner proposed {formatAccH(proposal.budget_accelerator_hours)} {unit} over {proposal.duration_hours} hours of research
            for {proposal.agents.length} {proposal.agents.length === 1 ? 'researcher' : 'researchers'}, after {proposal.baseline_minutes} minutes to measure the baseline; the figures are an estimate, since nothing has run, and the budget is a cap on everything the run spends, the baseline and burst hours included.
          </Text>
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
            <FormField label={`Budget (${unit})`} error={errorOf('budget')}><Input type="number" min={0} step="any" value={budget} disabled={!editable} onChange={e => setBudget(e.target.value)} /></FormField>
            <FormField label="Baseline (minutes)" error={errorOf('baseline')} hint="Until the baseline is due. The run starts once it is measured."><Input type="number" min={1} step={1} value={baselineMinutes} disabled={!editable} onChange={e => setBaselineMinutes(e.target.value)} /></FormField>
            <FormField label="Duration (hours)" error={errorOf('hours')} hint="Research time, from the start of the run."><Input type="number" min={0} step="any" value={hours} disabled={!editable} onChange={e => setHours(e.target.value)} /></FormField>
            <FormField label="Researchers" error={errorOf('researchers')}><Input type="number" min={1} step={1} value={researchers} disabled={!editable} onChange={e => setResearchers(e.target.value)} /></FormField>
          </div>
        </Captioned>
        )}
        <Sentences title="What could mislead" items={proposal.risks} />
        <Sentences title="Left out on purpose" items={proposal.out_of_scope} />
        <Captioned caption={!accepted ? 'The brief every researcher works from' : questions.length > 0 ? 'The brief as proposed, before the answers were added' : 'The brief as proposed'}>
          <CollapsibleDescription text={proposal.experiment.description} />
        </Captioned>
        {editable && (
          <Inline gap="md">
            <Button variant="primary" loading={sending} onClick={accept}>Accept and run</Button>
            {error && <InlineError>{error}</InlineError>}
          </Inline>
        )}
      </Stack>
    </Section>
    {children}
    </Stack>
  )
}
