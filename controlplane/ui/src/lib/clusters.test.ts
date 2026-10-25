import { expect, it } from 'vitest'
import { clusterLabel, clustersInUse } from '@/lib/clusters'
import type { ListedCluster } from '@/types/api'

const cluster = (over: Partial<ListedCluster>): ListedCluster => ({ name: 'c', enabled: true, created_at: '', owned: false, connected: true, autoscaler_enabled: false, actions: [], ...over })

// The home page told a new person "0/0 own clusters online, no clusters of your own" while two
// platform clusters were ready to run their jobs.
it('counts the clusters an org uses, the platform\'s with its own, and leaves out the ones switched off', () => {
  expect(clustersInUse([cluster({}), cluster({ connected: false }), cluster({ enabled: false })]))
    .toEqual({ used: 2, online: 1, whose: "2 of the platform's" })
  expect(clustersInUse([cluster({ owned: true }), cluster({})])).toEqual({ used: 2, online: 2, whose: "1 of your own, 1 of the platform's" })
  expect(clustersInUse([])).toEqual({ used: 0, online: 0, whose: '' })
})

it('names a cluster the platform rents on by its vendor, and any other by its own name', () => {
  expect(clusterLabel(cluster({ name: 'hypothesisloop-vast', provider: 'vast' }))).toBe('Vast.ai')
  expect(clusterLabel(cluster({ name: 'hypothesisloop-runpod', provider: 'runpod' }))).toBe('RunPod')
  expect(clusterLabel(cluster({ name: 'cloud-x', provider: 'acme' }))).toBe('cloud-x')
  expect(clusterLabel(cluster({ name: 'lab' }))).toBe('lab')
})
