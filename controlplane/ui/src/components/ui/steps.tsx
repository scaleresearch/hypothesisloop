import { clsx } from 'clsx'

/**
 * The steps of a course in order, with the one it stands at now. Nothing here moves by itself:
 * `current` is where the record says it stands, and `live` marks a step something is at work in.
 */
export function Steps({ label, steps, current, live }: {
  label: string
  steps: { key: string; label: string }[]
  current?: string
  live?: boolean
}) {
  const at = steps.findIndex(step => step.key === current)
  return (
    <ol aria-label={label} className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
      {steps.map((step, i) => (
        <li
          key={step.key}
          aria-current={i === at ? 'step' : undefined}
          className={clsx('flex items-center gap-2', i === at ? 'font-semibold text-accent' : i < at ? 'text-muted' : 'text-subtle')}
        >
          {i > 0 && <span aria-hidden="true" className={clsx('h-px w-5', i <= at ? 'bg-accent/50' : 'bg-border-strong')} />}
          <span
            aria-hidden="true"
            className={clsx('h-2 w-2 rounded-full', i === at ? 'bg-accent' : i < at ? 'bg-accent/50' : 'bg-border-strong', i === at && live && 'animate-pulse')}
          />
          {step.label}
        </li>
      ))}
    </ol>
  )
}
