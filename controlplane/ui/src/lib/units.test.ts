import { describe, expect, it } from 'vitest'
import { inFlight, occupancy, unitCount, unitRate } from './units'

describe('a count of a priced type', () => {
  it('says cores for a CPU class and is a bare number for an accelerator', () => {
    expect(unitCount('cpu', 16)).toBe('16 cores')
    expect(unitCount('cpu', 1)).toBe('1 core')
    expect(unitCount('cpu', 0)).toBe('0 cores')
    expect(unitCount('accelerator', 4)).toBe('4')
    // An API that predates kinds sends none: everything it lists is an accelerator.
    expect(unitCount(undefined, 4)).toBe('4')
  })

  it('prices a CPU class by the core-hour', () => {
    expect(unitRate('cpu', 0.01)).toBe('0.01 per core-hour')
    expect(unitRate('accelerator', 0.25)).toBe('0.25')
    expect(unitRate(undefined, 1)).toBe('1')
  })
})

describe('what an experiment has in flight', () => {
  it('reads as before when it holds no cores', () => {
    expect(inFlight(3, 0, null)).toEqual({ label: 'Accelerators In Flight', value: '3', atCap: false })
    expect(inFlight(3, 0, 8)).toEqual({ label: 'Accelerators In Flight', value: '3 / 8', atCap: false })
    expect(inFlight(8, 0, 8).atCap).toBe(true)
  })

  it('shows cores apart from accelerators and holds both against the cap', () => {
    expect(inFlight(2, 16, null)).toEqual({ label: 'Accelerators And Cores In Flight', value: '2 accelerators, 16 cores', atCap: false })
    expect(inFlight(2, 16, 64)).toEqual({ label: 'Accelerators And Cores In Flight', value: '2 accelerators, 16 cores / 64', atCap: false })
    expect(inFlight(2, 62, 64).atCap).toBe(true)
    expect(inFlight(0, 1, 64).value).toBe('0 accelerators, 1 core / 64')
  })
})

describe('occupancy of one kind', () => {
  it('is a share of what is installed, or nothing when none is', () => {
    expect(occupancy(3, 4, 'chips')).toEqual({ percent: '75%', detail: '3/4 chips busy, live across connected clusters' })
    expect(occupancy(6, 16, 'cores')).toEqual({ percent: '38%', detail: '6/16 cores busy, live across connected clusters' })
    expect(occupancy(0, 0, 'cores')).toEqual({ percent: '—', detail: null })
  })
})
