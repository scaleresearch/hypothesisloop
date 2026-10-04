import type { ReactNode } from 'react'

/** A few labelled values on one wrapping line: what a job runs on, where an experiment's code lives. */
export function Facts({ facts }: { facts: [string, ReactNode][] }) {
  return (
    <dl className="flex flex-wrap items-baseline gap-x-5 gap-y-1 text-xs">
      {facts.map(([label, value]) => (
        <div key={label} className="flex min-w-0 items-baseline gap-1.5">
          <dt className="text-muted">{label}</dt>
          <dd className="min-w-0 truncate font-mono">{value}</dd>
        </div>
      ))}
    </dl>
  )
}
