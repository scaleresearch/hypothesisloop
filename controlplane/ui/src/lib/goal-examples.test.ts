import { describe, expect, it } from 'vitest'
import { GOAL_EXAMPLES } from '@/lib/goal-examples'

describe('the goal examples', () => {
  it('are between 12 and 20 distinct sentences', () => {
    expect(GOAL_EXAMPLES.length).toBeGreaterThanOrEqual(12)
    expect(GOAL_EXAMPLES.length).toBeLessThanOrEqual(20)
    expect(new Set(GOAL_EXAMPLES).size).toBe(GOAL_EXAMPLES.length)
  })
})
