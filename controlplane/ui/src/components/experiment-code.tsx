import { clsx } from 'clsx'
import { textLinkClass } from '@/components/ui/link'
import { Text } from '@/components/ui/text'
import type { PlatformExperiment } from '@/types/api'

// The platform's own git server names a repository org_<uuid>/<name>; only the name is of interest.
function repositoryName(repository: string): string {
  return repository.replace(/^org_[0-9a-f-]+\//i, '')
}

/**
 * The repository a platform experiment works on, as owner/name, and the image its sandboxed agents
 * run when it is not the platform's own. Both are fixed at creation.
 */
export function ExperimentCode({ pe }: { pe: Pick<PlatformExperiment, 'repository' | 'repository_url' | 'agent_image'> }) {
  return (
    <>
      {pe.repository && (pe.repository_url
        ? <a href={pe.repository_url} target="_blank" rel="noreferrer" title={`${pe.repository} ${pe.repository_url}`} className={clsx(textLinkClass, 'max-w-xs truncate text-xs')}>{repositoryName(pe.repository)}</a>
        : <Text as="span" variant="note" className="max-w-xs truncate" title={pe.repository}>{repositoryName(pe.repository)}</Text>)}
      {pe.agent_image && (
        <Text as="span" variant="meta" mono className="max-w-xs truncate" title={pe.agent_image}>image {pe.agent_image}</Text>
      )}
    </>
  )
}
