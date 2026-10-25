'use client'

import { useState } from 'react'
import { useSWRConfig } from 'swr'
import { acceleratorLabel, formatAccH } from '@/lib/format'
import { budgetUnit } from '@/lib/units'
import { keys } from '@/lib/keys'
import { proposalOf } from '@/lib/planning'
import type { StoredProposal } from '@/lib/planning'
import { DEFAULT_AGENT_TYPE, typeName, useAgentRoles, useAgentTypes } from '@/lib/agent-types'
import { useTenant } from '@/lib/tenant'
import { ModelName } from '@/components/model-name'
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { Button } from '@/components/ui/button'
import { Captioned } from '@/components/ui/captioned'
import { InsetCard } from '@/components/ui/card'
import { CollapsibleDescription } from '@/components/ui/collapsible-description'
import { Disclosure } from '@/components/ui/disclosure'
import { Facts } from '@/components/ui/facts'
import { Section } from '@/components/ui/section'
import { Inline, Stack } from '@/components/ui/stack'
import { Pill, Text } from '@/components/ui/text'
import type { AgentRoleInfo, ListedAgentType, MetricDefinition, PlatformExperiment, ProposedAgent } from '@/types/api'

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

/** One agent of the team, read only: its role, type, model and direction. */
function Member({ agent, catalog, roles }: { agent: ProposedAgent; catalog: ListedAgentType[] | undefined; roles: AgentRoleInfo[] | undefined }) {
  const typeID = agent.agent_type || DEFAULT_AGENT_TYPE
  const role = roles?.find(r => r.role === catalog?.find(t => t.id === typeID)?.role)
  const direction = agent.hyperparameters?.direction as string | undefined
  return (
    <InsetCard as="li" gap="xs">
      <Inline gap="md" align="baseline" className="text-xs">
        {role && <Pill tone="accent" active>{role.name}</Pill>}
        <span className="text-muted">type <span className="text-foreground">{typeName(typeID, catalog)}</span></span>
        <span className="text-muted">model {agent.model ? <ModelName model={agent.model} className="text-foreground" /> : <span className="text-foreground">inherited</span>}</span>
      </Inline>
      <Text pre>{direction ?? 'No direction of its own: it works from the description.'}</Text>
    </InsetCard>
  )
}

/** The plan as it stands: the experiment's own fields once the plan applied, the planner's while it drafts. */
function standing(pe: PlatformExperiment, proposal: StoredProposal) {
  if (pe.status === 'draft') {
    return {
      metrics: proposal.experiment.metrics, description: proposal.experiment.description,
      budget: proposal.budget_accelerator_hours, hours: proposal.duration_hours, team: proposal.agents,
    }
  }
  const team: ProposedAgent[] = (pe.agents ?? []).flatMap(entry => Array.from({ length: entry.count }, () => ({
    agent_type: entry.agent_type, model: entry.model, hyperparameters: entry.hyperparameters,
  })))
  return { metrics: pe.metrics ?? [], description: pe.description ?? '', budget: pe.budget_accelerator_hours ?? 0, hours: pe.duration_hours ?? 0, team }
}

/**
 * The experiment's plan: what is measured, on what machine, at what cost and by whom, with what
 * the planner reasoned and assumed. While the planner drafts it, it is marked Draft. Once it
 * applied, Edit opens what the person may change of it now.
 */
export function Plan({ pe }: { pe: PlatformExperiment }) {
  const proposal = proposalOf(pe)
  const { mutate } = useSWRConfig()
  const [editing, setEditing] = useState(false)
  const { org } = useTenant()
  const { data: catalog } = useAgentTypes(org.id)
  const { data: roles } = useAgentRoles()
  if (!proposal) return null
  const plan = standing(pe, proposal)
  const ranked = plan.metrics.find(m => m.role !== 'constraint' && m.role !== 'reference' && m.role !== 'attribute') ?? plan.metrics[0]
  const unit = budgetUnit(proposal.accelerator_kind, 'long')
  const editable = pe.status !== 'draft' && pe.editable.some(field => field !== 'goal')
  const questions = proposal.questions ?? []
  const { stages, report_interval_seconds: reportEvery } = proposal.experiment

  return (
    <Section
      title="Plan"
      note={pe.status === 'draft' ? <Pill tone="accent">Draft</Pill> : undefined}
      actions={editable && <Button size="sm" onClick={() => setEditing(true)}>Edit</Button>}
    >
      {editing && <PlatformExperimentDialog initial={pe} onClose={() => setEditing(false)} onSaved={() => void mutate(keys.experiment(pe.id))} />}
      <Stack gap="md">
        <Facts facts={[
          ['Metric', ranked ? metricLine(ranked) : 'none'],
          [proposal.accelerator_kind === 'cpu' ? 'Runs on' : 'Machine', acceleratorLabel(proposal.accelerator_type)],
          [`Budget (${unit})`, formatAccH(plan.budget)],
          ['Duration', `${plan.hours} h`],
          ['Team', `${plan.team.length} agents`],
        ]} />
        <Disclosure label="Show the full plan">
          <Stack gap="lg" className="pt-2">
            <Captioned caption="What is measured">
              <Stack as="ul" gap="xs" className="mb-2">
                {plan.metrics.map(metric => (
                  <Inline as="li" align="baseline" key={metric.key}>
                    <Pill tone={metric.role === 'constraint' ? 'warning' : 'success'} active={metric.role !== 'constraint' && metric.role !== 'attribute'}>
                      {metricLine(metric)}
                    </Pill>
                  </Inline>
                ))}
              </Stack>
              <Text>{proposal.metric_reason}</Text>
            </Captioned>
            <Captioned caption="How it is measured">
              <Stack gap="sm">
                <Text>{proposal.measurement}</Text>
                <Facts facts={([['CPU', proposal.cpu], ['Memory', proposal.memory], ['Storage', proposal.storage]] as [string, string | undefined][])
                  .filter((entry): entry is [string, string] => !!entry[1])} />
              </Stack>
            </Captioned>
            <Captioned caption="Team">
              <ol className="grid gap-1.5">
                {plan.team.map((agent, i) => <Member key={i} agent={agent} catalog={catalog} roles={roles} />)}
              </ol>
            </Captioned>
            {questions.length > 0 && (
              <Captioned caption="Assumptions">
                <dl className="grid gap-3">
                  {questions.map((q, i) => (
                    <div key={i}>
                      <dt className="text-sm text-muted">{q.question}</dt>
                      <dd><Text>{pe.accepted_answers?.[i] ?? q.answer}</Text></dd>
                    </div>
                  ))}
                </dl>
              </Captioned>
            )}
            {(!!stages?.length || !!reportEvery) && (
              <Captioned caption="Rules of the run">
                <ul className={BULLETS}>
                  {stages?.map((stage, i) => (
                    <li key={i}>
                      Stage {i + 1}: {stage.length_pct}% of the run
                      {stage.evict_pct > 0 ? `, then the worst ${stage.evict_pct}% of executors are cut` : ', nobody is cut'}
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
            <Sentences title="What could mislead" items={proposal.risks} />
            <Sentences title="Left out on purpose" items={proposal.out_of_scope} />
            <Captioned caption="The description every agent works from">
              <CollapsibleDescription text={plan.description} />
            </Captioned>
          </Stack>
        </Disclosure>
      </Stack>
    </Section>
  )
}
