import { clsx } from 'clsx'
import type { PlatformExperiment } from '@/types'
import { textLinkClass } from '@/components/ui/link'
import { Text } from '@/components/ui/text'

/**
 * The repository a platform experiment works on, as owner/name, and the image its hosted agents
 * run when it is not the platform's own. Both are fixed at creation.
 */
export function ExperimentCode({ pe }: { pe: Pick<PlatformExperiment, 'repository' | 'repository_url' | 'agent_image'> }) {
  return (
    <>
      {pe.repository && (pe.repository_url
        ? <a href={pe.repository_url} target="_blank" rel="noreferrer" title={pe.repository_url} className={clsx(textLinkClass, 'max-w-xs truncate text-xs')}>{pe.repository}</a>
        : <Text as="span" variant="note" className="max-w-xs truncate">{pe.repository}</Text>)}
      {pe.agent_image && (
        <Text as="span" variant="meta" mono className="max-w-xs truncate" title={pe.agent_image}>image {pe.agent_image}</Text>
      )}
    </>
  )
}
