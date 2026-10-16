import { expect, it } from 'vitest'
import { clustersInUse } from '@/lib/clusters'
import type { OrgCluster } from '@/types/clusters'

const cluster = (over: Partial<OrgCluster>): OrgCluster => ({ name: 'c', shared: false, enabled: true, created_at: '', owned: false, connected: true, autoscaler_enabled: false, ...over })

// The home page told a new person "0/0 own clusters online, no clusters of your own" while two
// shared clusters were ready to run their jobs.
it('counts the clusters an org uses, the platform\'s with its own, and leaves out the ones switched off', () => {
  expect(clustersInUse([cluster({ shared: true }), cluster({ shared: true, connected: false }), cluster({ shared: true, enabled: false })]))
    .toEqual({ used: 2, online: 1, whose: "2 of the platform's" })
  expect(clustersInUse([cluster({ owned: true }), cluster({ shared: true })])).toEqual({ used: 2, online: 2, whose: "1 of your own, 1 of the platform's" })
  expect(clustersInUse([])).toEqual({ used: 0, online: 0, whose: '' })
})
