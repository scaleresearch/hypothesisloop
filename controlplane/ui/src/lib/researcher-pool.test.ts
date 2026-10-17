import { describe, expect, it } from 'vitest'
import { cutByMax, researchersAtWork, toPool } from '@/lib/researcher-pool'
import type { JoinedAgent } from '@/types/api'

function researcher(id: string, joined: string, more: Partial<JoinedAgent> = {}): JoinedAgent {
  return {
    platform_experiment_id: 'pe-1', agent_id: id, name: id, role: 'competitor', actor: { kind: 'user', id: 'u1', name: 'Ada' },
    in_agentsandbox: true, agent_type: 'generalist', model: 'm', hyperparameters: null, at_work: true, ended: '', signed_up_at: `2026-01-01T0${joined}:00:00Z`, actions: [], ...more,
  }
}

describe('the researchers at work, as the platform says', () => {
  it('keeps those the platform says are at work, oldest first', () => {
    const roster = [
      researcher('c', '3'),
      researcher('a', '1'),
      researcher('baseline', '0', { role: 'baseline', at_work: false }),
      researcher('done', '1', { at_work: false, ended_reason: 'finished' }),
      researcher('old', '1', { at_work: false, ended_reason: 'replaced' }),
      researcher('b', '2'),
    ]
    expect(researchersAtWork(roster).map(a => a.agent_id)).toEqual(['a', 'b', 'c'])
  })

  it('cuts the newest above a lowered max', () => {
    const atWork = [researcher('a', '1'), researcher('b', '2'), researcher('c', '3')]
    expect(cutByMax(atWork, 1).map(a => a.agent_id)).toEqual(['b', 'c'])
    expect(cutByMax(atWork, 3)).toEqual([])
  })
})

describe('sending the pool', () => {
  it('sends rows as entries, an empty model left out', () => {
    expect(toPool([{ agent_type: 'at-lr', model: ' ', count: '2' }])).toEqual([{ agent_type: 'at-lr', count: 2 }])
  })
})
