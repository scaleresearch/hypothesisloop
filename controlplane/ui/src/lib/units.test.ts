import { describe, expect, it } from 'vitest'
import { budgetKind, budgetUnit, CPU_TYPE, inFlight, kindOf, occupancy, unitCount, unitRate } from './units'

describe('a count of a priced type', () => {
  it('says cores for the CPU type and is a bare number for an accelerator', () => {
    expect(unitCount('cpu', 16)).toBe('16 cores')
    expect(unitCount('cpu', 1)).toBe('1 core')
    expect(unitCount('cpu', 0)).toBe('0 cores')
    expect(unitCount('accelerator', 4)).toBe('4')
    // An API that predates kinds sends none: everything it lists is an accelerator.
    expect(unitCount(undefined, 4)).toBe('4')
  })

  it('prices cores by the core-hour', () => {
    expect(unitRate('cpu', 1)).toBe('1 per core-hour')
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

describe('the unit of a budget', () => {
  const gpu = 'nvidia.com/gpu.product=NVIDIA-L40'

  it('is CPU hours for the CPU type, in any casing, and accelerator-hours for every other', () => {
    expect(budgetUnit(kindOf(CPU_TYPE))).toBe('CPU hours')
    expect(budgetUnit(kindOf('HypothesisLoop.dev/CPU=core'), 'long')).toBe('CPU core-hours')
    expect(budgetUnit(kindOf(gpu))).toBe('AccH')
    expect(budgetUnit(kindOf(undefined), 'long')).toBe('accelerator-hours')
  })

  it('follows the type an experiment was proposed for, and its jobs when nothing was proposed', () => {
    expect(budgetKind(CPU_TYPE, [gpu])).toBe('cpu')
    expect(budgetKind(gpu, [CPU_TYPE])).toBe('accelerator')
    expect(budgetKind(undefined, [CPU_TYPE, CPU_TYPE])).toBe('cpu')
    expect(budgetKind(undefined, [CPU_TYPE, gpu])).toBe('accelerator')
    expect(budgetKind(undefined, [])).toBe('accelerator')
  })
})
