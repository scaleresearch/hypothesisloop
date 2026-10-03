import { describe, expect, it } from 'vitest'
import { GOAL_EXAMPLES, matchingGoals } from '@/lib/goal-examples'

describe('matchingGoals', () => {
  it('keeps between 12 and 20 distinct examples', () => {
    expect(GOAL_EXAMPLES.length).toBeGreaterThanOrEqual(12)
    expect(GOAL_EXAMPLES.length).toBeLessThanOrEqual(20)
    expect(new Set(GOAL_EXAMPLES).size).toBe(GOAL_EXAMPLES.length)
  })

  it('matches each typed word to the start of a word of the example, in any case', () => {
    expect(matchingGoals('sota LIDAR compr')).toEqual(['Beat the SOTA bits-per-point on lidar point cloud compression for KITTI'])
    expect(matchingGoals('lidar', 5)).toEqual([
      'Beat the SOTA bits-per-point on lidar point cloud compression for KITTI',
      'Find the voxel size that maximises PointPillars mAP on nuScenes lidar',
    ])
    expect(matchingGoals('the robomimic')).toEqual(['Cut grasp-success failures of a diffusion policy on the RoboMimic Lift task'])
  })

  it('returns none when a word matches nothing, or only small words were typed', () => {
    expect(matchingGoals('lidar sourdough')).toEqual([])
    expect(matchingGoals('the')).toEqual([])
  })

  it('leaves out the example already in the box, and never gives more than asked', () => {
    expect(matchingGoals(GOAL_EXAMPLES[0])).toEqual([])
    expect(matchingGoals('does', 2)).toHaveLength(2)
    expect(matchingGoals('   ')).toEqual(GOAL_EXAMPLES.slice(0, 3))
  })
})
