import { beforeEach, describe, expect, it, vi } from 'vitest'
import { addPlatformExperimentAgents, createPlatformExperiment, updatePlatformExperiment } from '@/lib/api'
import type { CreatePlatformExperimentRequest } from '@/lib/api'
import { clearRunFailure, createAndRun, runFailure } from '@/lib/run-experiment'
import type { PlatformExperiment } from '@/types'

vi.mock('@/lib/api', () => ({ addPlatformExperimentAgents: vi.fn(), createPlatformExperiment: vi.fn(), updatePlatformExperiment: vi.fn() }))

const request = { name: 'run', repository: 'acme/widgets', starts_at: '2026-10-06T10:00:00.000Z', ends_at: '2026-10-13T10:00:00.000Z' } as CreatePlatformExperimentRequest
const experiment = { id: 'pe-1', name: 'run' } as PlatformExperiment
let calls: string[]

beforeEach(() => {
  calls = []
  clearRunFailure('pe-1')
  vi.mocked(createPlatformExperiment).mockImplementation(async () => { calls.push('create'); return experiment })
  vi.mocked(addPlatformExperimentAgents).mockImplementation(async () => { calls.push('agent'); return [] })
  vi.mocked(updatePlatformExperiment).mockReset().mockImplementation(async () => { calls.push('start time'); return experiment })
})

describe('creating and running a platform experiment', () => {
  // It once called start, which is refused until a baseline is declared. It states the start
  // time, and the platform starts the experiment from then once its coordinator declared one.
  it('creates it with a hosted coordinator and no scheduled start, adds one hosted agent a call, then states its start time as now', async () => {
    await expect(createAndRun('prj_1', request, 3)).resolves.toEqual({ experiment })
    expect(calls).toEqual(['create', 'agent', 'agent', 'agent', 'start time'])
    expect(createPlatformExperiment).toHaveBeenCalledExactlyOnceWith('prj_1', {
      name: 'run', repository: 'acme/widgets', ends_at: '2026-10-13T10:00:00.000Z', coordinator_hosted: true,
    })
    for (const call of vi.mocked(addPlatformExperimentAgents).mock.calls) expect(call).toEqual(['pe-1', { hosted: true }])
    expect(updatePlatformExperiment).toHaveBeenCalledExactlyOnceWith('pe-1', { starts_at: expect.stringMatching(/^\d{4}-\d\d-\d\dT/) })
    expect(runFailure('pe-1')).toBeUndefined()
  })

  it('makes each call only once the one before it has answered', async () => {
    let release = () => {}
    vi.mocked(addPlatformExperimentAgents).mockImplementationOnce(() => new Promise(resolve => { release = () => resolve([]) }))
    const run = createAndRun('prj_1', request, 2)
    await vi.waitFor(() => expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(1))
    await new Promise(resolve => setTimeout(resolve, 20))
    expect(addPlatformExperimentAgents).toHaveBeenCalledTimes(1)
    expect(updatePlatformExperiment).not.toHaveBeenCalled()
    release()
    await run
    expect(updatePlatformExperiment).toHaveBeenCalledTimes(1)
  })

  it('rejects with the API\'s refusal when the create is refused, and calls nothing else', async () => {
    vi.mocked(createPlatformExperiment).mockRejectedValue(new Error('the org connected no installation on that account'))
    await expect(createAndRun('prj_1', request, 2)).rejects.toThrow('the org connected no installation on that account')
    expect(addPlatformExperimentAgents).not.toHaveBeenCalled()
    expect(updatePlatformExperiment).not.toHaveBeenCalled()
  })

  it('stops at the agent the API refuses, names it, and neither starts nor tries again nor undoes', async () => {
    vi.mocked(addPlatformExperimentAgents).mockImplementationOnce(async () => { calls.push('agent'); return [] })
      .mockImplementationOnce(async () => { calls.push('agent'); throw new Error('the platform runs at most 2 machines for one platform experiment') })
    const failure = { step: 'adding hosted agent 2 of 3', message: 'the platform runs at most 2 machines for one platform experiment' }
    await expect(createAndRun('prj_1', request, 3)).resolves.toEqual({ experiment, failure })
    expect(calls).toEqual(['create', 'agent', 'agent'])
    expect(runFailure('pe-1')).toEqual(failure)
  })

  it('stops at a refused start time with every agent added, and keeps the refusal for the experiment\'s page', async () => {
    vi.mocked(updatePlatformExperiment).mockRejectedValue(new Error('starts_at must be before ends_at'))
    const failure = { step: 'setting its start time', message: 'starts_at must be before ends_at' }
    await expect(createAndRun('prj_1', request, 1)).resolves.toEqual({ experiment, failure })
    expect(calls).toEqual(['create', 'agent'])
    expect(runFailure('pe-1')).toEqual(failure)
    clearRunFailure('pe-1')
    expect(runFailure('pe-1')).toBeUndefined()
  })
})
