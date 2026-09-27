import { beforeEach, describe, expect, it, vi } from 'vitest'
import { addPlatformExperimentAgents, createPlatformExperiment } from '@/lib/api'
import type { CreatePlatformExperimentRequest } from '@/lib/api'
import { clearRunFailure, createAndRun, runFailure } from '@/lib/run-experiment'
import type { PlatformExperiment } from '@/types'

vi.mock('@/lib/api', () => ({ addPlatformExperimentAgents: vi.fn(), createPlatformExperiment: vi.fn() }))

const request = { name: 'run', repository: 'acme/widgets', starts_at: '2026-10-06T10:00:00.000Z', duration_hours: 48 } as CreatePlatformExperimentRequest
const experiment = { id: 'pe-1', name: 'run' } as PlatformExperiment
let calls: string[]

beforeEach(() => {
  calls = []
  clearRunFailure('pe-1')
  vi.mocked(createPlatformExperiment).mockImplementation(async () => { calls.push('create'); return experiment })
  vi.mocked(addPlatformExperimentAgents).mockImplementation(async () => { calls.push('agent'); return [] })
})

describe('creating and running a platform experiment', () => {
  // It once stated a start time after the agents. An experiment with none starts by itself once
  // its coordinator declared a baseline.
  it('creates it with a hosted coordinator and no scheduled start, adds one hosted agent a call, and makes no other call', async () => {
    await expect(createAndRun('prj_1', request, 3)).resolves.toEqual({ experiment })
    expect(calls).toEqual(['create', 'agent', 'agent', 'agent'])
    expect(createPlatformExperiment).toHaveBeenCalledExactlyOnceWith('prj_1', {
      name: 'run', repository: 'acme/widgets', duration_hours: 48, coordinator_hosted: true,
    })
    for (const call of vi.mocked(addPlatformExperimentAgents).mock.calls) expect(call).toEqual(['pe-1', { hosted: true }])
    expect(runFailure('pe-1')).toBeUndefined()
  })

  it('makes each call only once the one before it has answered', async () => {
    let release = () => {}
    vi.mocked(addPlatformExperimentAgents).mockImplementationOnce(() => new Promise(resolve => { release = () => resolve([]) }))
    const run = createAndRun('prj_1', request, 2)
    await vi.waitFor(() => expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(1))
    await new Promise(resolve => setTimeout(resolve, 20))
    expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(1)
    release()
    await run
    expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(2)
  })

  it('rejects with the API\'s refusal when the create is refused, and calls nothing else', async () => {
    vi.mocked(createPlatformExperiment).mockRejectedValue(new Error('the org connected no installation on that account'))
    await expect(createAndRun('prj_1', request, 2)).rejects.toThrow('the org connected no installation on that account')
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
  })

  it('stops at the agent the API refuses, names it, keeps the refusal for the experiment\'s page, and neither tries again nor undoes', async () => {
    vi.mocked(addPlatformExperimentAgents).mockImplementationOnce(async () => { calls.push('agent'); return [] })
      .mockImplementationOnce(async () => { calls.push('agent'); throw new Error('the platform runs at most 2 machines for one platform experiment') })
    const failure = { step: 'adding hosted agent 2 of 3', message: 'the platform runs at most 2 machines for one platform experiment' }
    await expect(createAndRun('prj_1', request, 3)).resolves.toEqual({ experiment, failure })
    expect(calls).toEqual(['create', 'agent', 'agent'])
    expect(runFailure('pe-1')).toEqual(failure)
    clearRunFailure('pe-1')
    expect(runFailure('pe-1')).toBeUndefined()
  })
})
