import type { OrgCluster } from '@/types'

/**
 * What an org's jobs can run on: the clusters it uses, its own and the platform's, and how many
 * of them are connected. A cluster the org switched off is not one of them.
 */
export function clustersInUse(clusters: OrgCluster[]) {
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

const MAX_PRIORITY = 1000

/** Moves the item at `from` to `to`, as a new array. */
export function moved<T>(list: T[], from: number, to: number): T[] {
  const out = [...list]
  out.splice(to, 0, ...out.splice(from, 1))
  return out
}

/**
 * The priorities to write so that the org's own clusters stand in `order`, first placed first,
 * after `name` was moved. A priority is 1 to 1000, higher first.
 *
 * The moved cluster takes the whole number halfway between its new neighbours (1001 above the
 * first, 0 below the last), which is one write. When no whole number lies between them, as with
 * clusters that all hold the default, every cluster is spaced evenly over the range (1000 x
 * (n - i) / (n + 1), rounded, for the i-th of n) and only those whose value changes are
 * written. Evenly spaced values leave room for many single-write moves before the next respacing.
 */
export function priorityWrites(order: Pick<OrgCluster, 'name' | 'priority'>[], name: string): { name: string; priority: number }[] {
  const at = order.findIndex(c => c.name === name)
  const above = at === 0 ? MAX_PRIORITY + 1 : order[at - 1].priority
  const below = at === order.length - 1 ? 0 : order[at + 1].priority
  const between = Math.floor((above + below) / 2)
  if (between < above && between > below) return between === order[at].priority ? [] : [{ name, priority: between }]
  const n = order.length
  return order
    .map((c, i) => ({ name: c.name, priority: Math.round((MAX_PRIORITY * (n - i)) / (n + 1)), was: c.priority }))
    .filter(c => c.priority !== c.was)
    .map(({ name, priority }) => ({ name, priority }))
}
