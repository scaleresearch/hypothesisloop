import { describe, expect, it } from 'vitest'
import { cutByMax, defaultTeam, executorsAtWork, toTeam, toTeamRows } from '@/lib/team'
import type { JoinedAgent } from '@/types/api'

function agent(id: string, joined: string, more: Partial<JoinedAgent> = {}): JoinedAgent {
  return {
    platform_experiment_id: 'pe-1', agent_id: id, name: id, role: 'competitor', actor: { kind: 'user', id: 'u1', name: 'Ada' },
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

  it('sends rows as entries, an empty model and empty hyperparameters left out', () => {
    expect(toTeam([{ agent_type: 'at-lr', model: ' ', count: '2', hyperparameters: ' ' }])).toEqual({ agents: [{ agent_type: 'at-lr', count: 2 }] })
    expect(toTeam([{ agent_type: 'generalist', model: 'openrouter/qwen/qwen3.7-flash', count: '1', hyperparameters: '{"direction": "kernels"}' }]))
      .toEqual({ agents: [{ agent_type: 'generalist', model: 'openrouter/qwen/qwen3.7-flash', count: 1, hyperparameters: { direction: 'kernels' } }] })
  })

  it('names the entry whose hyperparameters are no JSON object, and sends nothing', () => {
    expect(toTeam([{ agent_type: 'generalist', model: '', count: '1', hyperparameters: '' }, { agent_type: 'generalist', model: '', count: '1', hyperparameters: '[1]' }]))
      .toEqual({ problem: 'Team entry 2: hyperparameters: A JSON object, as {"lr": 0.001}.' })
  })

  it('reads entries back as rows, hyperparameters as JSON text', () => {
    expect(toTeamRows([{ count: 2 }, { agent_type: 'researcher', count: 1, hyperparameters: { lr: 0.1 } }])).toEqual([
      { agent_type: 'generalist', count: '2', model: '', hyperparameters: '' },
      { agent_type: 'researcher', count: '1', model: '', hyperparameters: '{\n  "lr": 0.1\n}' },
    ])
  })
})
