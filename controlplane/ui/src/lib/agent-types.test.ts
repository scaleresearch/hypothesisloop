import { describe, expect, it } from 'vitest'
import { describeType, parseHyperparameters, teamTypes, typeName } from '@/lib/agent-types'
import { CATALOG, ROLES, agentType } from '@/test-agent-types'

describe('reading hyperparameters as typed', () => {
  it('takes hyperparameters as a JSON object, empty text as none', () => {
    expect(parseHyperparameters('')).toEqual({ value: {} })
    expect(parseHyperparameters('{"lr": 0.1}')).toEqual({ value: { lr: 0.1 } })
    expect(parseHyperparameters('[1]')).toEqual({ problem: 'A JSON object, as {"lr": 0.001}.' })
    expect(parseHyperparameters('{lr')).toEqual({ problem: 'Not valid JSON.' })
  })
})

describe('naming and describing types', () => {
  it('describes a type by the description it states, and an org\'s own that states none by its role and author', () => {
    expect(describeType(CATALOG[1])).toMatch(/tunes the knobs/)
    expect(describeType(CATALOG.find(t => t.id === 'at-lr')!)).toBe("Your org's own executor type, made by Ada.")
    expect(describeType(agentType('at-x', 'X', 'executor', { description: 'Sweeps lr.' }))).toBe('Sweeps lr.')
  })

  it('reads an id as its name, and an id the catalog does not hold as itself', () => {
    expect(typeName('data-search', CATALOG)).toBe('Data search')
    expect(typeName('at-gone', CATALOG)).toBe('at-gone')
  })

  it('offers the types of the roles a team names, not archived, executors first and built-in first', () => {
    const archived = agentType('at-old', 'Old', 'executor', { archived_at: '2026-02-01T00:00:00Z' })
    const lr = CATALOG.find(t => t.id === 'at-lr')!
    const ids = teamTypes([lr, archived, ...CATALOG.filter(t => t !== lr)], ROLES).map(t => t.id)
    expect(ids).toEqual(['generalist', 'hyperparameter-search', 'architecture-search', 'data-search', 'at-lr', 'researcher'])
  })
})
