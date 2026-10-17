'use client'

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { isReadOnly, ReadOnlyProvider } from '@/lib/read-only'
import { projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { AgentNames } from '@/components/agent-label'
import { Baseline } from '@/components/baseline'
import { CloseExperiment } from '@/components/close-experiment'
import { DraftView } from '@/components/draft'
import { ExperimentAgents } from '@/components/experiment-agents'
import { ExperimentCode } from '@/components/experiment-code'
import { GoalEditor } from '@/components/goal-editor'
import { HostedMachines } from '@/components/hosted-machines'
import { PhaseStrip } from '@/components/phase-strip'
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { ReferenceValues } from '@/components/reference-values'
import { RemoveExperiment } from '@/components/remove-experiment'
import { ResearcherPool } from '@/components/researcher-pool'
import { SharedBadge } from '@/components/shared-badge'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Disclosure } from '@/components/ui/disclosure'
import { Figures } from '@/components/ui/figures'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Inline } from '@/components/ui/stack'
import { ErrorMessage, Loading } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { runClock } from '@/lib/format'
import { phaseOf } from '@/lib/phase'
import { useRoster } from '@/lib/roster'
import { budgetKind, budgetUnit } from '@/lib/units'
import { proposalOf } from '@/lib/planning'
import { About } from './_overview/about'
import { overviewFigures } from './_overview/figures'
import { Findings } from './_overview/findings'
import { LiveMetricsPicker } from './_overview/job-metrics'
import { JobsTable } from './_overview/jobs-table'
import { Leaderboard } from './_overview/leaderboard'
import { Outcome } from './_overview/outcome'
import { ResearchersOverTime } from './_overview/researchers-over-time'
import { useOverview } from './_overview/use-overview'
import type { PlatformExperiment } from '@/types/api'

// The reasons a draft closes for, before anything was accepted.
const PLANNED_ONLY: NonNullable<PlatformExperiment['close_reason']>[] = ['planning_limit_reached', 'proposal_expired', 'planner_stopped', 'repository_not_created']

export default function PlatformExperimentDetailPage({ params }: { params: { experimentId: string } }) {
  return <AgentNames platformExperimentID={params.experimentId}><PlatformExperimentDetail id={params.experimentId} /></AgentNames>
}

function PlatformExperimentDetail({ id }: { id: string }) {
  const router = useRouter()
  const [editing, setEditing] = useState(false)
  const { me, project } = useTenant()
  const reads = useOverview(id)
  const { data: pe, error: peError, isLoading: peLoading, mutate: mutatePE } = reads.experiment
  const { data: roster } = useRoster(pe)
  const { quotas, experiments, stats, results, findings, stages } = reads

  if (peLoading) return <Loading />
  if (peError) return <ErrorMessage>Could not load this platform experiment: {peError.message}</ErrorMessage>
  if (pe?.status === 'draft') return <DraftView draft={pe} readOnly={isReadOnly(me, pe)} />
  if (reads.failed.length > 0) return <ErrorMessage>Could not load {reads.failed.join(', ')} for this platform experiment.</ErrorMessage>
  if (!pe) return <ErrorMessage>Experiment not found.</ErrorMessage>

  const metrics = pe.metrics ?? []
  const jobsByAgent = stats?.groups ?? {}
  // Every agent known to this platform experiment, whether via quota signup or a submitted job.
  const knownAgentIds = Array.from(new Set([
    ...(quotas ?? []).map(q => q.agent_id),
    ...Object.keys(jobsByAgent),
  ])).sort()
  const primary = results?.metrics[0]
  const hypothesisText = new Map((findings ?? []).map(h => [h.id, h.text]))
  const unit = budgetUnit(budgetKind(proposalOf(pe)?.accelerator_type, (experiments ?? []).map(exp => exp.accelerator_type)))
  const clock = runClock(pe)
  // One list: what is running first, then the newest of the rest.
  const running = reads.running ?? []
  const jobRows = [...running, ...(experiments ?? []).filter(exp => !running.some(r => r.id === exp.id))]

  const readOnly = isReadOnly(me, pe)
  // Nobody is ranked and nothing is charted before the run starts: until then the phase strip says what happens.
  const phase = phaseOf(pe)
  const started = phase === 'running' || phase === 'closed'
  // A draft that closed never had researchers: its max_agents is still the most its planner could propose.
  const plannedOnly = pe.close_reason != null && PLANNED_ONLY.includes(pe.close_reason)
  const figures = overviewFigures({
    pe, phase, plannedOnly, unit, quotas, stats,
    hypothesisStats: reads.hypothesisStats,
    verdicts: reads.verdicts,
    researchers: roster?.filter(agent => agent.role === 'competitor').length,
  })
  const findingsSection = <Findings experimentId={id} findings={findings ?? []} direction={primary?.direction} />

  return (
    <ReadOnlyProvider value={readOnly}>
    <Page
      back={{ label: 'Experiments', onClick: () => router.push(projectPath(project.id, 'experiments')) }}
      title={pe.name}
      actions={!readOnly && (
        <Inline gap="sm">
          {pe.editable?.some(field => field !== 'goal') && <Button size="sm" onClick={() => setEditing(true)}>Edit</Button>}
          {pe.status !== 'closed' && <CloseExperiment pe={pe} onClosed={() => void mutatePE()} />}
          <RemoveExperiment pe={pe} onRemoved={() => router.push(projectPath(project.id, 'experiments'))} />
        </Inline>
      )}
      description={
        <Inline gap="md">
          <Badge status={pe.status}>{pe.status}</Badge>
          <SharedBadge pe={pe} />
          <ExperimentCode pe={pe} />
          {clock && <Text as="span" variant="hint">{clock}</Text>}
        </Inline>
      }
    >
      {editing && <PlatformExperimentDialog initial={pe} onClose={() => setEditing(false)} onSaved={() => void mutatePE()} />}
      <PhaseStrip pe={pe} jobs={jobRows} stages={stages} readOnly={readOnly}>
        <HostedMachines pe={pe} jobs={stats?.total ?? 0} />
      </PhaseStrip>

      <Baseline pe={pe} />
      <Figures label="This experiment in figures" figures={figures} />

      {phase === 'closed' && <Outcome pe={pe} results={results} hypothesisText={hypothesisText} />}
      {phase === 'closed' && findingsSection}

      {started && (
        <Leaderboard
          pe={pe}
          agentIds={knownAgentIds}
          results={results}
          quotas={quotas}
          stages={stages}
          jobsByAgent={jobsByAgent}
          hypothesisText={hypothesisText}
          unit={unit}
        />
      )}

      {results?.references && <ReferenceValues experimentId={id} references={results.references} />}

      {phase === 'running' && findingsSection}
      {started && metrics.length > 0 && (
        <ResearchersOverTime metrics={metrics} agentIds={knownAgentIds} series={reads.metricSeries} boundaries={stages?.advances} />
      )}

      {!plannedOnly && <ResearcherPool pe={pe} />}
      <ExperimentAgents pe={pe} />

      {pe.goal && <GoalEditor pe={pe} onSaved={() => void mutatePE()} />}
      <About pe={pe} />

      {jobRows.length > 0 && (
        <Section title="Job metrics" note="over training progress, refreshed every 30 s">
          <Disclosure label="Show the metrics of one job">
            <LiveMetricsPicker jobs={jobRows} />
          </Disclosure>
        </Section>
      )}

      <JobsTable experimentId={id} jobs={jobRows} total={stats?.total ?? 0} rankingMetric={primary?.metric} />
    </Page>
    </ReadOnlyProvider>
  )
}
