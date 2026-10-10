import type { StagesStatus } from '@/lib/api'
import { Stack } from '@/components/ui/stack'
import { Text } from '@/components/ui/text'

const percent = (share: number) => Math.round(share * 100)

// Where each stage but the last ends, as a share of the run.
function boundaries(stages: StagesStatus['stages']): number[] {
  let at = 0
  return stages.slice(0, -1).map(stage => (at += stage.length_pct / 100))
}

function sentence({ stages, current_stage: current, progress, next_boundary_progress: next, cut_agents: cut }: StagesStatus): string {
  const parts = [`${percent(progress)}% of the run is done.`]
  if (stages.length > 1) parts.unshift(`Stage ${current} of ${stages.length}.`)
  if (current < stages.length) {
    const evict = stages[current - 1].evict_pct
    parts.push(evict > 0
      ? `At ${percent(next)}% the platform cuts ${evict}% of the researchers still in it.`
      : `The next stage starts at ${percent(next)}%.`)
  }
  if (cut.length > 0) parts.push(`${cut.length} cut so far.`)
  return parts.join(' ')
}

// The rules of the stages in words, for the bar's title.
function rules(stages: StagesStatus['stages']): string {
  return stages.map((stage, i) => {
    const end = i === stages.length - 1 ? 'runs to the end' : stage.evict_pct > 0 ? `ends with a cut of ${stage.evict_pct}% of the researchers` : 'ends with no cut'
    const length = stage.max_job_hours ? `, and no job in it may run longer than ${stage.max_job_hours} h` : ''
    return `Stage ${i + 1} is ${stage.length_pct}% of the run and ${end}${length}.`
  }).join(' ')
}

/**
 * How far a started run is, on one bar: the share of its time used and a mark where each stage
 * ends. Which researcher is ahead is not here, as stages publish their boundaries alone.
 */
export function RunProgress({ status }: { status: StagesStatus }) {
  const done = percent(status.progress)
  return (
    <Stack gap="xs">
      <div
        role="progressbar"
        aria-label="Run"
        aria-valuenow={done}
        aria-valuemin={0}
        aria-valuemax={100}
        title={rules(status.stages)}
        className="relative h-1.5 overflow-hidden rounded-full bg-track"
      >
        <div className="h-full rounded-full bg-accent transition-all" style={{ width: `${done}%` }} />
        {boundaries(status.stages).map((at, i) => (
          <span
            key={i}
            className="absolute inset-y-0 w-0.5 bg-surface"
            style={{ left: `${percent(at)}%` }}
          />
        ))}
      </div>
      <Text variant="note">{sentence(status)}</Text>
    </Stack>
  )
}
