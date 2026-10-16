import { Fragment } from 'react'
import { AGENT_TYPES } from '@/lib/agent-types'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Stack } from '@/components/ui/stack'
import { Eyebrow } from '@/components/ui/text'
import { ONE_STARTING_AGENT } from '@/components/platform-experiment-form/model'
import type { StartingAgentDraft } from '@/components/platform-experiment-form/model'

/** The agents a new experiment starts with, one row per agent type and model. */
export function StartingAgentsEditor({ rows, onChange }: { rows: StartingAgentDraft[]; onChange: (rows: StartingAgentDraft[]) => void }) {
  function setRow(i: number, patch: Partial<StartingAgentDraft>) {
    onChange(rows.map((r, idx) => idx === i ? { ...r, ...patch } : r))
  }

  return (
    <Stack gap="sm">
      <div className="grid grid-cols-[minmax(0,2fr)_5rem_minmax(0,2fr)_auto] items-center gap-2">
        <Eyebrow>Type</Eyebrow>
        <Eyebrow>Count</Eyebrow>
        <Eyebrow>Model</Eyebrow>
        <span />
        {rows.map((r, i) => (
          <Fragment key={i}>
            <Select
              aria-label={`Starting agent ${i + 1} type`}
              value={r.agent_type}
              options={AGENT_TYPES.map(t => ({ value: t, label: t }))}
              onChange={agent_type => setRow(i, { agent_type })}
            />
            <Input
              className="w-full min-w-0" type="number" min={1} step={1} value={r.count}
              aria-label={`Starting agent ${i + 1} count`}
              onChange={e => setRow(i, { count: e.target.value })}
            />
            <Input
              className="w-full min-w-0" value={r.model}
              aria-label={`Starting agent ${i + 1} model`}
              placeholder="platform default"
              onChange={e => setRow(i, { model: e.target.value })}
            />
            <Button
              type="button" variant="ghost" size="icon"
              onClick={() => onChange(rows.filter((_, idx) => idx !== i))}
              disabled={rows.length <= 1}
              title="Remove this row"
            >×</Button>
          </Fragment>
        ))}
      </div>
      <div>
        <Button type="button" size="sm" onClick={() => onChange([...rows, ONE_STARTING_AGENT])}>+ Add agents</Button>
      </div>
    </Stack>
  )
}
