import { expect, it } from 'vitest'
import { clustersInUse, priorityWrites } from '@/lib/clusters'
import type { OrgCluster } from '@/types'

const cluster = (over: Partial<OrgCluster>): OrgCluster => ({ name: 'c', shared: false, enabled: true, priority: 100, created_at: '', owned: false, connected: true, ...over })

// The home page told a new person "0/0 own clusters online, no clusters of your own" while two
// shared clusters were ready to run their jobs.
it('counts the clusters an org uses, the platform\'s with its own, and leaves out the ones switched off', () => {
  expect(clustersInUse([cluster({ shared: true }), cluster({ shared: true, connected: false }), cluster({ shared: true, enabled: false })]))
    .toEqual({ used: 2, online: 1, whose: "2 of the platform's" })
  expect(clustersInUse([cluster({ owned: true }), cluster({ shared: true })])).toEqual({ used: 2, online: 2, whose: "1 of your own, 1 of the platform's" })
  expect(clustersInUse([])).toEqual({ used: 0, online: 0, whose: '' })
})

it('writes one priority for a move where a whole number fits between the neighbours, and respaces all where none does', () => {
  const at = (name: string, priority: number) => ({ name, priority })
  expect(priorityWrites([at('a', 750), at('c', 250), at('b', 500)], 'c')).toEqual([{ name: 'c', priority: 625 }])
  expect(priorityWrites([at('a', 750), at('b', 500), at('c', 250)], 'c')).toEqual([])
  expect(priorityWrites([at('a', 1000), at('c', 5), at('b', 999)], 'c')).toEqual([{ name: 'a', priority: 750 }, { name: 'c', priority: 500 }, { name: 'b', priority: 250 }])
  expect(priorityWrites([at('only', 100)], 'only')).toEqual([{ name: 'only', priority: 500 }])
})
