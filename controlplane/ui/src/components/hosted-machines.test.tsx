import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { machinesStatus } from '@/components/hosted-machines'
import type { AgentInstance } from '@/types'

const NOW = new Date('2026-10-05T12:00:00Z')
const ago = (seconds: number) => new Date(NOW.getTime() - seconds * 1000).toISOString()
const machine = (over: Partial<AgentInstance> = {}): AgentInstance => ({ state: 'running', restarts: 0, stopped: false, ...over })

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(NOW)
})
afterEach(() => vi.useRealTimers())

// The minutes after Create and run showed empty charts and a table of zeros, with the state of
// the machines a screen further down.
describe('what the machines of a hosted experiment are doing', () => {
  it('says that machines are coming up while none has made progress', () => {
    const status = machinesStatus([machine({ state: 'requested' }), machine({ state: 'starting' }), machine({ state: 'starting' })], 0)
    expect(status?.now).toBe('The platform runs 3 machines for this experiment: 2 starting, 1 asked for.')
    expect(status?.next).toMatch(/boots and clones the repository in about a minute/)
  })

  it('says the agents are at work once one has made progress and no job exists', () => {
    const status = machinesStatus([machine({ progress_at: ago(200) }), machine({ progress_at: ago(20) }), machine({ state: 'starting' })], 0)
    expect(status?.now).toBe('The platform runs 3 machines for this experiment: 2 running, 1 starting.')
    expect(status?.next).toMatch(/^Last progress 20s ago\. The agents are reading the brief/)
  })

  it('is short once jobs exist, points at the reasons when every machine stopped, and is absent with no machine', () => {
    expect(machinesStatus([machine({ progress_at: ago(60) })], 2)).toEqual({ now: 'The platform runs 1 machine for this experiment: 1 running.', next: 'Last progress 1m ago.' })
    expect(machinesStatus([machine({ state: 'stopped', stopped: true, progress_at: ago(60) })], 2)?.next).toMatch(/None is asked for again/)
    expect(machinesStatus([], 0)).toBeNull()
  })
})
