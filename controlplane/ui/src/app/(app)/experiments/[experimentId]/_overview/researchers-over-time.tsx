'use client'

import { useMemo, useState } from 'react'
import { clsx } from 'clsx'
import type { AgentMetricSeries, MetricDefinition, StagesStatus } from '@/types/platform-experiments'
import { EmptyState } from '@/components/ui/empty-state'
import { Section } from '@/components/ui/section'
import { Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'
import { semantic, seriesPalette } from '@/lib/colors'
import { AgentSelector, MetricSelector } from './chart-selectors'
import { CompetingAgentsChart } from './competing-chart'

// Colors are assigned once per full (unfiltered) agent roster, keyed by a sorted id list, so a
// given agent keeps the same color even as the user toggles which agents/metrics are visible.
function useAgentColorMap(agentIds: string[]): Map<string, string> {
  const roster = [...agentIds].sort().join(',')
  return useMemo(
    () => new Map(roster.split(',').map((id, i) => [id, seriesPalette[i % seriesPalette.length]])),
    [roster],
  )
}

/** Each researcher's best so far on every declared metric, over the run. */
export function ResearchersOverTime({ metrics, agentIds, series, boundaries }: {
  metrics: MetricDefinition[]
  agentIds: string[]
  series: { key: string; series: AgentMetricSeries[] }[] | undefined
  boundaries: StagesStatus['advances'] | undefined
}) {
  const [selectedAgentIds, setSelectedAgentIds] = useState<Set<string> | null>(null)
  const [selectedMetricKeys, setSelectedMetricKeys] = useState<Set<string> | null>(null)
  const colorMap = useAgentColorMap(agentIds)
  const colorFor = (agentId: string) => colorMap.get(agentId) ?? semantic.neutral

  // Both selectors show everything until the user narrows them down.
  const activeAgentIds = selectedAgentIds ?? new Set(agentIds)
  const activeMetricKeys = selectedMetricKeys ?? new Set(metrics.map(m => m.key))

  return (
    <Section title="Researchers over time">
      <div className={clsx('mb-4 grid gap-6', agentIds.length > 0 ? 'grid-cols-2' : 'grid-cols-1')}>
        <MetricSelector metrics={metrics} selected={activeMetricKeys} onChange={setSelectedMetricKeys} />
        {agentIds.length > 0 && (
          <AgentSelector agentIds={agentIds} selected={activeAgentIds} onChange={setSelectedAgentIds} colorFor={colorFor} />
        )}
      </div>
      {activeMetricKeys.size === 0 ? (
        <EmptyState>Select at least one metric to plot.</EmptyState>
      ) : (
        <Stack gap="lg">
          {metrics.filter(m => activeMetricKeys.has(m.key)).map(m => (
            <div key={m.key}>
              <Text as="div" variant="note" tone="default" className="mb-1 font-medium">
                {m.key} <span className="font-normal text-subtle">({m.direction})</span>
              </Text>
              <CompetingAgentsChart
                series={(series ?? []).find(e => e.key === m.key)?.series}
                metricName={m.key}
                direction={m.direction}
                selectedAgents={activeAgentIds}
                colorFor={colorFor}
                boundaries={boundaries}
              />
            </div>
          ))}
        </Stack>
      )}
    </Section>
  )
}
