import { describe, expect, it } from 'vitest'
import type { ExperimentStats } from '@/lib/api'
import { hypothesisActivity } from '@/lib/hypothesis-progress'

const stats = (status: string) => ({ total: 1, by_status: { [status]: 1 } }) as unknown as ExperimentStats

describe('the job activity of an experiment\'s hypotheses', () => {
  it('leaves out the baseline\'s jobs, which are grouped under the experiment\'s id and test no hypothesis', () => {
    expect(hypothesisActivity(3, { 'pe-1': stats('RUNNING'), h1: stats('RUNNING'), h2: stats('COMPLETED') }, 'pe-1'))
      .toBe('1 with jobs in flight · 1 without jobs')
    expect(hypothesisActivity(0, { 'pe-1': stats('RUNNING') }, 'pe-1')).toBe('')
  })
})
