import { Fragment } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Inline, Stack } from '@/components/ui/stack'
import { Eyebrow, Text } from '@/components/ui/text'
import { MAX_STAGES, round2, stagesError } from '@/components/platform-experiment-form/model'
import type { StageDraft } from '@/components/platform-experiment-form/model'

/** A custom stage ladder, one row per stage, with its total and what is wrong with it. */
export function StagesEditor({ stages, onChange }: { stages: StageDraft[]; onChange: (s: StageDraft[]) => void }) {
  const err = stagesError(stages)
  const total = round2(stages.reduce((s, x) => s + Number(x.length_pct || 0), 0))

  function setStage(i: number, patch: Partial<StageDraft>) {
    onChange(stages.map((s, idx) => idx === i ? { ...s, ...patch } : s))
  }

  return (
    <Stack gap="sm">
      <div className="grid grid-cols-ladder items-center gap-2">
        <span />
        <Eyebrow>Length %</Eyebrow>
        <Eyebrow>Evict %</Eyebrow>
        <Eyebrow>Max job hours</Eyebrow>
        <span />
        {stages.map((s, i) => {
          const last = i === stages.length - 1
          return (
            <Fragment key={i}>
              <Text as="span" variant="hint" className="tabular-nums">{i + 1}</Text>
              <Input
                className="w-full min-w-0" type="number" min={0.1} max={100} step="any" value={s.length_pct}
                aria-label={`Stage ${i + 1} length %`}
                onChange={e => setStage(i, { length_pct: e.target.value })}
              />
              {last ? (
                <Text as="span" tone="muted" className="px-3" title="Nothing follows the final stage">0</Text>
              ) : (
                <Input
                  className="w-full min-w-0" type="number" min={0} max={99} step={5} value={s.evict_pct}
                  aria-label={`Stage ${i + 1} evict %`}
                  onChange={e => setStage(i, { evict_pct: e.target.value })}
                />
              )}
              <Input
                className="w-full min-w-0" type="number" min={0} step={0.5} value={s.max_job_hours}
                aria-label={`Stage ${i + 1} max job hours`}
                placeholder="no limit"
                onChange={e => setStage(i, { max_job_hours: e.target.value })}
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                onClick={() => onChange(stages.filter((_, idx) => idx !== i).map((st, idx, arr) => idx === arr.length - 1 ? { ...st, evict_pct: '0' } : st))}
                disabled={stages.length <= 1}
                title="Remove this stage"
              >×</Button>
            </Fragment>
          )
        })}
      </div>

      <Inline gap="md">
        <Button
          type="button" size="sm"
          disabled={stages.length >= MAX_STAGES}
          onClick={() => onChange([
            ...stages.slice(0, -1),
            { ...stages[stages.length - 1], evict_pct: '25' },
            { length_pct: '', evict_pct: '0', max_job_hours: '' },
          ])}
        >+ Add stage</Button>
        <Text as="span" variant="note" tone={Math.abs(total - 100) < 0.001 ? 'success' : 'warning'} className="tabular-nums">
          Total {total}%
        </Text>
      </Inline>

      {err && <Text variant="note" tone="warning">{err}</Text>}
    </Stack>
  )
}
