import { describe, expect, it } from 'vitest'
import { cutByMax, defaultTeam, executorCount, executorsAtWork, keptRows, startsFrom, toTeam, toTeamRows } from '@/lib/team'
import type { JoinedAgent } from '@/types/api'

function agent(id: string, joined: string, more: Partial<JoinedAgent> = {}): JoinedAgent {
  return {
    platform_experiment_id: 'pe-1', agent_id: id, name: id, role: 'executor', actor: { kind: 'user', id: 'u1', name: 'Ada' },
    in_agentsandbox: true, agent_type: 'generalist', model: 'anthropic/claude-sonnet-5', hyperparameters: null, at_work: true, ended: '', signed_up_at: `2026-01-01T0${joined}:00:00Z`, actions: [], ...more,
  }
}

describe('the executors at work, as the platform says', () => {
  it('keeps the executors the platform says are at work, oldest first, and no researcher', () => {
    const roster = [
      agent('c', '3'),
      agent('a', '1'),
      agent('baseline', '0', { role: 'baseline', at_work: false }),
      agent('done', '1', { at_work: false, ended_reason: 'finished' }),
      agent('old', '1', { at_work: false, ended_reason: 'replaced' }),
      agent('r', '1', { role: 'researcher', agent_type: 'researcher' }),
      agent('b', '2'),
    ]
    expect(executorsAtWork(roster).map(a => a.agent_id)).toEqual(['a', 'b', 'c'])
  })

  it('cuts the newest above a lowered max', () => {
    const atWork = [agent('a', '1'), agent('b', '2'), agent('c', '3')]
    expect(cutByMax(atWork, 1).map(a => a.agent_id)).toEqual(['b', 'c'])
    expect(cutByMax(atWork, 3)).toEqual([])
  })
})

describe('sending the team', () => {
  it('starts a new team with executors of the default type and one researcher', () => {
    expect(toTeam(defaultTeam(3))).toEqual({ agents: [{ agent_type: 'generalist', count: 3 }, { agent_type: 'researcher', count: 1 }] })
  })

  it('sends rows as entries, an empty model left out and hyperparameters as they came', () => {
    expect(toTeam([{ agent_type: 'at-lr', model: ' ', count: '2' }])).toEqual({ agents: [{ agent_type: 'at-lr', count: 2 }] })
    expect(toTeam([{ agent_type: 'generalist', model: 'openrouter/qwen/qwen3.7-flash', count: '1', hyperparameters: { direction: 'kernels' } }]))
      .toEqual({ agents: [{ agent_type: 'generalist', model: 'openrouter/qwen/qwen3.7-flash', count: 1, hyperparameters: { direction: 'kernels' } }] })
  })

  it('names the entry whose count is no whole number of at least 1, and sends nothing', () => {
    for (const count of ['', '0', '1.5', 'x']) {
      expect(toTeam([{ agent_type: 'generalist', model: '', count: '1' }, { agent_type: 'researcher', model: '', count }]))
        .toEqual({ problem: 'Team entry 2: the count is a whole number of at least 1.' })
    }
  })

  it('names a second entry of one type and model, unless each row is one agent', () => {
    const rows = [{ agent_type: 'generalist', model: '', count: '1' }, { agent_type: 'data-search', model: '', count: '1' }, { agent_type: 'generalist', model: ' ', count: '2' }]
    expect(toTeam(rows)).toEqual({ problem: 'Team entry 3: entry 1 has this type and model. Raise its count instead.' })
    expect(toTeam(rows, { perAgent: true })).toMatchObject({ agents: [{}, {}, {}] })
    expect(toTeam([{ agent_type: 'generalist', model: '', count: '1' }, { agent_type: 'generalist', model: 'anthropic/claude-sonnet-5', count: '1' }])).toMatchObject({ agents: [{}, {}] })
  })

  it('reads entries back as rows, hyperparameters kept as they are', () => {
    expect(toTeamRows([{ count: 2, hyperparameters: {} }, { agent_type: 'researcher', count: 1, hyperparameters: { lr: 0.1 } }])).toEqual([
      { agent_type: 'generalist', count: '2', model: '' },
      { agent_type: 'researcher', count: '1', model: '', hyperparameters: { lr: 0.1 } },
    ])
  })

  it('reads rows a draft kept with hyperparameters as JSON text', () => {
    expect(keptRows([
      { agent_type: 'generalist', count: '2', model: '', hyperparameters: '' },
      { agent_type: 'generalist', count: '1', model: 'm', hyperparameters: '{"direction": "kernels"}' },
      { agent_type: 'data-search', count: '1', model: '', hyperparameters: '{oops' },
    ])).toEqual([
      { agent_type: 'generalist', count: '2', model: '' },
      { agent_type: 'generalist', count: '1', model: 'm', hyperparameters: { direction: 'kernels' } },
      { agent_type: 'data-search', count: '1', model: '' },
    ])
  })

  it('counts every agent but a researcher as an executor, and says what an entry starts from', () => {
    expect(executorCount([{ agent_type: 'generalist', count: '2', model: '' }, { agent_type: 'at-lr', count: '3', model: '' }, { agent_type: 'researcher', count: '4', model: '' }])).toBe(5)
    expect(startsFrom({ agent_type: 'generalist', count: '1', model: '', hyperparameters: { direction: 'kernels', lr: 1 } })).toBe('kernels')
    expect(startsFrom({ agent_type: 'generalist', count: '1', model: '', hyperparameters: { direction: 3 } })).toBe('{"direction":3}')
    expect(startsFrom({ agent_type: 'generalist', count: '1', model: '' })).toBe('')
  })
})
