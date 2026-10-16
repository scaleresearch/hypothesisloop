'use client'

import { AgentLabel } from '@/components/agent-label'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dot } from '@/components/ui/dot'
import { Inline, Stack } from '@/components/ui/stack'
import { Eyebrow } from '@/components/ui/text'

export function AgentSelector({
  agentIds, selected, onChange, colorFor,
}: {
  agentIds: string[]
  selected: Set<string>
  onChange: (next: Set<string>) => void
  colorFor: (agentId: string) => string
}) {
  const allSelected = agentIds.length > 0 && agentIds.every(id => selected.has(id))
  return (
    <Stack gap="sm">
      <Inline nowrap className="justify-between">
        <Eyebrow>Agents</Eyebrow>
        <Button type="button" variant="link" size="sm" onClick={() => onChange(allSelected ? new Set() : new Set(agentIds))}>
          {allSelected ? 'Clear' : 'Select all'}
        </Button>
      </Inline>
      <Inline gap="xs">
        {agentIds.map((id) => {
          const isOn = selected.has(id)
          return (
            <Checkbox
              key={id}
              pill
              color={colorFor(id)}
              checked={isOn}
              onChange={() => {
                const next = new Set(selected)
                isOn ? next.delete(id) : next.add(id)
                onChange(next)
              }}
              label={<><Dot color={colorFor(id)} /><AgentLabel id={id} /></>}
            />
          )
        })}
      </Inline>
    </Stack>
  )
}

export function MetricSelector({
  metrics, selected, onChange,
}: {
  metrics: { key: string; direction: string }[]
  selected: Set<string>
  onChange: (next: Set<string>) => void
}) {
  const allSelected = metrics.length > 0 && metrics.every(m => selected.has(m.key))
  return (
    <Stack gap="sm">
      <Inline nowrap className="justify-between">
        <Eyebrow>Metrics</Eyebrow>
        <Button type="button" variant="link" size="sm" onClick={() => onChange(allSelected ? new Set() : new Set(metrics.map(m => m.key)))}>
          {allSelected ? 'Clear' : 'Select all'}
        </Button>
      </Inline>
      <Inline gap="xs">
        {metrics.map((m, i) => {
          const isOn = selected.has(m.key)
          return (
            <Checkbox
              key={m.key}
              pill
              checked={isOn}
              onChange={() => {
                const next = new Set(selected)
                isOn ? next.delete(m.key) : next.add(m.key)
                onChange(next)
              }}
              label={<>
                {i === 0 && <span className="font-medium">★</span>}
                {m.key}
                <span>{m.direction === 'maximize' ? '↑' : '↓'}</span>
              </>}
            />
          )
        })}
      </Inline>
    </Stack>
  )
}
