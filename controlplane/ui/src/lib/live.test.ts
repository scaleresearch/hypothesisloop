import { describe, expect, it } from 'vitest'
import type { Change } from '@/types/api'
import { LIVE_KINDS, readsOf } from '@/lib/live'

const change = (kind: Change['kind'], value: string, detail: string) =>
  ({ kind, subject: 'h-dup', value, detail, platform_experiment_id: 'pe-1' }) as Change

describe('readsOf', () => {
  it('reads the page of the hypothesis a merge folded into', () => {
    const reads = readsOf([change('hypothesis.status', 'merged', 'h-kept')])
    expect(reads(['hypothesis', 'h-kept'])).toBe(true)
    expect(reads(['hypothesis', 'h-dup'])).toBe(true)
  })

  it('reads no other hypothesis for a verdict', () => {
    expect(readsOf([change('hypothesis.status', 'confirmed', 'why')])(['hypothesis', 'why'])).toBe(false)
  })

  it('follows an unlink on both ends of the link', () => {
    expect(LIVE_KINDS).toContain('hypothesis.unlink')
    const reads = readsOf([change('hypothesis.unlink', 'related', 'h-other')])
    expect(reads(['hypothesis', 'h-other'])).toBe(true)
    expect(reads(['hypothesis-graph', 'pe-1'])).toBe(true)
  })
})
