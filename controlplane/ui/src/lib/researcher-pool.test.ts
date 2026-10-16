import { describe, expect, it } from 'vitest'
import { cutByMax, poolProblem, researchersAtWork, toPool, toRows } from '@/lib/researcher-pool'
import type { JoinedAgent } from '@/types'

function researcher(id: string, joined: string, more: Partial<JoinedAgent> = {}): JoinedAgent {
  return {
    platform_experiment_id: 'pe-1', agent_id: id, name: id, role: 'competitor', actor: { kind: 'user', id: 'u1', name: 'Ada' },
    hosted: true, flavor: 'generalist', model: 'm', signed_up_at: `2026-01-01T0${joined}:00:00Z`, ...more,
  }
}

describe('the researchers at work, as the platform counts them against max_agents', () => {
  it('leaves out other roles and anyone finished, replaced, cut or without a machine, oldest first', () => {
    const roster = [
      researcher('c', '3'),
      researcher('a', '1'),
      researcher('baseline', '0', { role: 'baseline' }),
      researcher('done', '1', { finished_at: '2026-01-01T05:00:00Z' }),
      researcher('old', '1', { replaced_by: 'c' }),
      researcher('lowered', '2', { cut_reason: 'max_agents_lowered' }),
      researcher('staged', '2'),
      researcher('lost', '2', { instance: { state: 'stopped', restarts: 3, stopped: true } }),
      researcher('b', '2'),
    ]
    expect(researchersAtWork(roster, new Set(['staged'])).map(a => a.agent_id)).toEqual(['a', 'b', 'c'])
  })

  it('cuts the newest above a lowered max', () => {
    const atWork = [researcher('a', '1'), researcher('b', '2'), researcher('c', '3')]
    expect(cutByMax(atWork, 1).map(a => a.agent_id)).toEqual(['b', 'c'])
    expect(cutByMax(atWork, 3)).toEqual([])
  })
})

describe('checking a change of the pool as the API does', () => {
  const name = (id: string) => id
  const atWork = [researcher('a', '1'), researcher('b', '2', { flavor: 'data-search' })]

  it('passes a change of max alone, and refuses a max below 1', () => {
    expect(poolProblem(1, null, atWork, name)).toBeNull()
    expect(poolProblem(0, null, atWork, name)).toMatch(/at least 1/)
  })

  it('refuses a pool above max, two entries of one type and model, and an entry below its researchers at work', () => {
    const rows = toRows([{ agent_type: 'generalist', model: 'm', count: 2 }, { agent_type: 'data-search', model: 'm', count: 1 }])
    expect(poolProblem(3, rows, atWork, name)).toBeNull()
    expect(poolProblem(2, rows, atWork, name)).toBe('The pool wants 3 researchers, more than the max of 2.')
    expect(poolProblem(5, [...rows, rows[0]], atWork, name)).toMatch(/same type and model/)
    const fewer = [{ ...rows[0] }, { ...rows[1], count: '0' }]
    expect(poolProblem(5, fewer, atWork, name)).toMatch(/count of at least 1/)
    const below = toRows([{ agent_type: 'generalist', model: 'm', count: 1 }])
    expect(poolProblem(5, below, [...atWork, researcher('c', '3')], name)).toBe('2 generalist researchers on m are at work: keep at least 2. Lower max researchers to cut the newest.')
  })

  it('sends rows as entries, an empty model left out', () => {
    expect(toPool([{ agent_type: 'at-lr', model: ' ', count: '2' }])).toEqual([{ agent_type: 'at-lr', count: 2 }])
  })
})
