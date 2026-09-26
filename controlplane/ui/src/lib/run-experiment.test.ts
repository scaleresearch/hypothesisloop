import { beforeEach, describe, expect, it, vi } from 'vitest'
import { addPlatformExperimentAgents, createPlatformExperiment, startPlatformExperiment } from '@/lib/api'
import type { CreatePlatformExperimentRequest } from '@/lib/api'
import { clearRunFailure, createAndRun, runFailure } from '@/lib/run-experiment'
import type { PlatformExperiment } from '@/types'

vi.mock('@/lib/api', () => ({ addPlatformExperimentAgents: vi.fn(), createPlatformExperiment: vi.fn(), startPlatformExperiment: vi.fn() }))

const request = { name: 'run', repository: 'acme/widgets', starts_at: '2026-10-06T10:00:00.000Z', ends_at: '2026-10-13T10:00:00.000Z' } as CreatePlatformExperimentRequest
const experiment = { id: 'pe-1', name: 'run' } as PlatformExperiment
let calls: string[]

beforeEach(() => {
  calls = []
  clearRunFailure('pe-1')
  vi.mocked(createPlatformExperiment).mockImplementation(async () => { calls.push('create'); return experiment })
  vi.mocked(addPlatformExperimentAgents).mockImplementation(async () => { calls.push('agent'); return [] })
  vi.mocked(startPlatformExperiment).mockImplementation(async () => { calls.push('start') })
})

describe('creating and running a platform experiment', () => {
  it('creates it with a hosted coordinator and no scheduled start, adds one hosted agent a call, then starts it', async () => {
    await expect(createAndRun('prj_1', request, 3)).resolves.toEqual({ experiment })
    expect(calls).toEqual(['create', 'agent', 'agent', 'agent', 'start'])
    expect(createPlatformExperiment).toHaveBeenCalledExactlyOnceWith('prj_1', {
      ...request, starts_at: '0001-01-01T00:00:00Z', coordinator_hosted: true,
    })
    for (const call of vi.mocked(addPlatformExperimentAgents).mock.calls) expect(call).toEqual(['pe-1', { hosted: true }])
    expect(startPlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1')
    expect(runFailure('pe-1')).toBeUndefined()
  })

  it('makes each call only once the one before it has answered', async () => {
    let release = () => {}
    vi.mocked(addPlatformExperimentAgents).mockImplementationOnce(() => new Promise(resolve => { release = () => resolve([]) }))
    const run = createAndRun('prj_1', request, 2)
    await vi.waitFor(() => expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(1))
    await new Promise(resolve => setTimeout(resolve, 20))
    expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(1)
    expect(startPlatformExperiment).not.toHaveBeenCalled()
    release()
    await run
    expect(startPlatformExperiment).toHaveBeenCalledTimes(1)
  })

  it('rejects with the API\'s refusal when the create is refused, and calls nothing else', async () => {
    vi.mocked(createPlatformExperiment).mockRejectedValue(new Error('the org connected no installation on that account'))
    await expect(createAndRun('prj_1', request, 2)).rejects.toThrow('the org connected no installation on that account')
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
    expect(startPlatformExperiment).not.toHaveBeenCalled()
  })

  it('stops at the agent the API refuses, names it, and neither starts nor tries again nor undoes', async () => {
    vi.mocked(addPlatformExperimentAgents).mockImplementationOnce(async () => { calls.push('agent'); return [] })
      .mockImplementationOnce(async () => { calls.push('agent'); throw new Error('the platform runs at most 2 machines for one platform experiment') })
    const failure = { step: 'adding hosted agent 2 of 3', message: 'the platform runs at most 2 machines for one platform experiment' }
    await expect(createAndRun('prj_1', request, 3)).resolves.toEqual({ experiment, failure })
    expect(calls).toEqual(['create', 'agent', 'agent'])
    expect(runFailure('pe-1')).toEqual(failure)
  })

  it('stops at a refused start with every agent added, and keeps the refusal for the experiment\'s page', async () => {
    vi.mocked(startPlatformExperiment).mockRejectedValue(new Error('invalid_transition: experiment is draft, expected open'))
    const failure = { step: 'starting it', message: 'invalid_transition: experiment is draft, expected open' }
    await expect(createAndRun('prj_1', request, 1)).resolves.toEqual({ experiment, failure })
    expect(calls).toEqual(['create', 'agent'])
    expect(runFailure('pe-1')).toEqual(failure)
    clearRunFailure('pe-1')
    expect(runFailure('pe-1')).toBeUndefined()
  })
})
