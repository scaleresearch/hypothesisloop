'use client'

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { projectPath } from '@/lib/routes'
import { useTenant } from '@/lib/tenant'
import { AgentNames } from '@/components/agent-label'
import { Baseline } from '@/components/baseline'
import { CloseExperiment } from '@/components/close-experiment'
import { AbandonDraft, DraftSections } from '@/components/draft'
import { ExperimentCode } from '@/components/experiment-code'
import { GoalEditor } from '@/components/goal-editor'
import { AgentSandboxMachines } from '@/components/agentsandbox-machines'
import { PhaseStrip } from '@/components/phase-strip'
import { Plan } from '@/components/plan'
import { PlatformExperimentDialog } from '@/components/platform-experiment-form'
import { Button } from '@/components/ui/button'
import { ReferenceValues } from '@/components/reference-values'
import { RemoveExperiment } from '@/components/remove-experiment'
import { SharedBadge } from '@/components/shared-badge'
import { Badge } from '@/components/ui/badge'
import { Disclosure } from '@/components/ui/disclosure'
import { Figures } from '@/components/ui/figures'
import { Page } from '@/components/ui/page'
import { Section } from '@/components/ui/section'
import { Inline } from '@/components/ui/stack'
import { ErrorMessage, Loading } from '@/components/ui/status-message'
import { Text } from '@/components/ui/text'
import { runClock } from '@/lib/format'
import { closedWhilePlanned, phaseOf } from '@/lib/phase'
import { useRoster } from '@/lib/roster'
import { executorsAtWork } from '@/lib/team'
import { budgetUnit } from '@/lib/units'
import { useBudgetReference } from '@/lib/capacity'
import { About } from './_overview/about'
import { overviewFigures } from './_overview/figures'
import { Findings } from './_overview/findings'
import { LiveMetricsPicker } from './_overview/job-metrics'
import { JobsTable } from './_overview/jobs-table'
import { Leaderboard } from './_overview/leaderboard'
import { Outcome } from './_overview/outcome'
import { ExecutorsOverTime } from './_overview/executors-over-time'
import { useOverview } from './_overview/use-overview'
import type { PlatformExperiment } from '@/types/api'

export default function PlatformExperimentDetailPage({ params }: { params: { experimentId: string } }) {
  return <AgentNames platformExperimentID={params.experimentId}><PlatformExperimentDetail id={params.experimentId} /></AgentNames>
}

function PlatformExperimentDetail({ id }: { id: string }) {
  const reference = useBudgetReference()
  const router = useRouter()
  const [editing, setEditing] = useState(false)
  const { project } = useTenant()
  const reads = useOverview(id)
  const { data: pe, error: peError, isLoading: peLoading, mutate: mutatePE } = reads.experiment
  const { data: roster } = useRoster(pe)
  const { quotas, experiments, stats, results, findings, stages } = reads

  if (peLoading) return <Loading />
  if (peError) return <ErrorMessage>Could not load this platform experiment: {peError.message}</ErrorMessage>
  if (pe?.status === 'draft') {
    return (
      <Page
        back={{ label: 'Experiments', onClick: () => router.push(projectPath(project.id, 'experiments')) }}
        title={pe.name}
        actions={pe.actions.includes('abandon') && <AbandonDraft pe={pe} onAbandoned={() => void mutatePE()} />}
        description={<Inline gap="md"><Badge status={pe.status}>{pe.status}</Badge><ExperimentCode pe={pe} /></Inline>}
      >
        <DraftSections pe={pe} onChanged={() => void mutatePE()} />
      </Page>
    )
  }
  if (reads.failed.length > 0) return <ErrorMessage>Could not load {reads.failed.join(', ')} for this platform experiment.</ErrorMessage>
  if (!pe) return <ErrorMessage>Experiment not found.</ErrorMessage>

  const metrics = pe.metrics ?? []
  const jobsByAgent = stats?.groups ?? {}
  // Every agent known to this platform experiment, whether via quota signup or a submitted job.
  const knownAgentIds = Array.from(new Set([
    ...(quotas ?? []).map(q => q.agent_id),
    ...Object.keys(jobsByAgent),
  ])).sort()
  const executorIds = knownAgentIds.filter(agentId => roster?.some(agent => agent.agent_id === agentId && agent.role === 'executor'))
  const primary = results?.metrics[0]
  const hypothesisText = new Map((findings ?? []).map(h => [h.id, h.text]))
  const unit = budgetUnit(pe.budget_kind, 'short', reference)
  const clock = runClock(pe)
  // One list: what is running first, then the newest of the rest.
  const running = reads.running ?? []
  const jobRows = [...running, ...(experiments ?? []).filter(exp => !running.some(r => r.id === exp.id))]

  // Nobody is ranked and nothing is charted before the run starts: until then the phase strip says what happens.
  const phase = phaseOf(pe)
  const started = phase === 'running' || phase === 'closed'
  // A draft that closed never had a team: its max_agents is still the most its planner could propose.
  const plannedOnly = closedWhilePlanned(pe)
  const figures = overviewFigures({
    pe, phase, plannedOnly, unit, stats,
    hypothesisStats: reads.hypothesisStats,
    verdicts: reads.verdicts,
    executors: roster && (phase === 'closed' ? roster.filter(a => a.role === 'executor').length : executorsAtWork(roster).length),
  })
  const findingsSection = <Findings experimentId={id} findings={findings ?? []} direction={primary?.direction} />

  return (
    <Page
      back={{ label: 'Experiments', onClick: () => router.push(projectPath(project.id, 'experiments')) }}
      title={pe.name}
      actions={(
        <Inline gap="sm">
          {!pe.proposal && pe.editable.some(field => field !== 'goal') && <Button size="sm" onClick={() => setEditing(true)}>Edit</Button>}
          {pe.actions.includes('close') && <CloseExperiment pe={pe} onClosed={() => void mutatePE()} />}
          {pe.actions.includes('remove') && <RemoveExperiment pe={pe} onRemoved={() => router.push(projectPath(project.id, 'experiments'))} />}
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
      <PhaseStrip pe={pe} jobs={jobRows} stages={stages}>
        <AgentSandboxMachines pe={pe} jobs={stats?.total ?? 0} />
      </PhaseStrip>
      <Plan pe={pe} />

      <Baseline pe={pe} />
      <Figures label="This experiment in figures" figures={figures} />

      {phase === 'closed' && <Outcome pe={pe} results={results} hypothesisText={hypothesisText} />}

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

      {started && metrics.length > 0 && (
        <ExecutorsOverTime metrics={metrics} agentIds={executorIds} series={reads.metricSeries} boundaries={stages?.advances} />
      )}

      {started && findingsSection}

      {jobRows.length > 0 && (
        <Section title="Job metrics" note="over training progress, refreshed every 30 s">
          <Disclosure label="Show the metrics of one job">
            <LiveMetricsPicker jobs={jobRows} />
          </Disclosure>
        </Section>
      )}

      <JobsTable experimentId={id} jobs={jobRows} total={stats?.total ?? 0} rankingMetric={primary?.metric} />

      {pe.goal && <GoalEditor pe={pe} onSaved={() => void mutatePE()} />}
      {!pe.proposal && <About pe={pe} />}
    </Page>
  )
}
