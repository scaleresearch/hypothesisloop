import type { ListedCluster } from '@/types/api'

/**
 * What an org's jobs can run on: the clusters it uses, its own and the platform's, and how many
 * of them are connected. A cluster the org switched off is not one of them.
 */
export function clustersInUse(clusters: ListedCluster[]) {
  const used = clusters.filter(c => c.enabled)
  const online = used.filter(c => c.connected)
  const own = used.filter(c => c.owned).length
  const parts = [own > 0 && `${own} of your own`, used.length - own > 0 && `${used.length - own} of the platform's`].filter(Boolean)
  return {
    used: used.length,
    online: online.length,
    /** "1 of your own, 2 of the platform's", empty when no cluster is in use. */
    whose: parts.join(', '),
  }
}
