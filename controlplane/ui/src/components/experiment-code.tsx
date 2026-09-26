import type { PlatformExperiment } from '@/types'

/** The code a platform experiment works on and the image its hosted agents run, both fixed at creation. */
export function ExperimentCode({ pe }: { pe: Pick<PlatformExperiment, 'repository' | 'repository_url' | 'agent_image'> }) {
  return (
    <dl className="flex flex-wrap items-baseline gap-x-5 gap-y-1 text-xs">
      <div className="flex items-baseline gap-1.5">
        <dt className="text-subtle">Repository</dt>
        <dd className="font-mono">
          {pe.repository
            ? pe.repository_url
              ? <a href={pe.repository_url} target="_blank" rel="noreferrer" className="text-accent hover:underline">{pe.repository}</a>
              : pe.repository
            : <span className="font-sans text-muted">none</span>}
        </dd>
      </div>
      <div className="flex items-baseline gap-1.5">
        <dt className="text-subtle">Agent image</dt>
        <dd className="font-mono">{pe.agent_image || <span className="font-sans text-muted">the platform&apos;s</span>}</dd>
      </div>
    </dl>
  )
}
