import { describe, expect, it } from 'vitest'
import {
  MAX_PROMPT_BYTES, describeType, parseHyperparameters, placeholdersOf, promptProblems, researcherTypes, settingsProblems, typeName,
} from '@/lib/agent-types'
import { CATALOG, RESEARCHER_PROMPT, agentType } from '@/test-agent-types'

describe('the placeholders of a role, as domain.AgentPlaceholders', () => {
  it('gives a researcher its type and hyperparameters, a baseline and a planner an agent id, a coordinator neither', () => {
    const names = (role: Parameters<typeof placeholdersOf>[0]) => placeholdersOf(role).map(p => p.name)
    const common = ['rules', 'hl_commands', 'workdir', 'platform_experiment_id', 'repository_url']
    expect(names('researcher')).toEqual([...common, 'agent_id', 'agent_type', 'hyperparameters'])
    expect(names('baseline')).toEqual([...common, 'agent_id'])
    expect(names('planner')).toEqual([...common, 'agent_id'])
    expect(names('coordinator')).toEqual(common)
  })
})

describe('checking a prompt as domain.ValidatePrompt does', () => {
  it('passes a prompt with both required placeholders and only ones its role has', () => {
    expect(promptProblems('researcher', RESEARCHER_PROMPT)).toEqual([])
  })

  it('names every required placeholder that is missing', () => {
    expect(promptProblems('researcher', 'Just text')).toEqual(['{{rules}} is required.', '{{hl_commands}} is required.'])
  })

  it('names a placeholder the role has no value for, each once', () => {
    const prompt = '{{rules}} {{hl_commands}} {{agent_id}} {{agent_id}} {{secret}}'
    expect(promptProblems('coordinator', prompt)).toEqual([
      '{{agent_id}} has no value in a coordinator prompt.',
      '{{secret}} has no value in a coordinator prompt.',
    ])
  })

  it('refuses braces that are no placeholder', () => {
    expect(promptProblems('researcher', '{{rules}} {{hl_commands}} {{ Not one }}')).toEqual([
      'A {{ or }} that is no placeholder. A placeholder is {{name}} in lowercase letters and _.',
    ])
  })

  it('refuses an empty prompt and one over the byte limit', () => {
    expect(promptProblems('planner', '  \n')).toEqual(['The prompt is empty.'])
    const long = `{{rules}} {{hl_commands}} ${'é'.repeat(MAX_PROMPT_BYTES / 2)}`
    expect(promptProblems('planner', long)[0]).toMatch(/^The prompt is \d+ bytes, over the limit of 65536\.$/)
  })
})

describe('checking settings as domain.AgentTypeSettings.Validate does', () => {
  const ok = { model: '', max_subagents: 4, pacing_subagents: 2, effort: 'high' as const }

  it('passes the built-in settings and an empty model', () => {
    expect(settingsProblems(ok)).toEqual({})
  })

  it('holds subagents to 1..10 and pacing to 1..max, and a model to its pattern', () => {
    expect(settingsProblems({ ...ok, max_subagents: 11 }).max_subagents).toBe('1 to 10.')
    expect(settingsProblems({ ...ok, pacing_subagents: 5 }).pacing_subagents).toBe('1 to 4.')
    expect(settingsProblems({ ...ok, model: 'has space' }).model).toMatch(/letters, digits/)
    expect(settingsProblems({ ...ok, model: 'claude-opus-4-5[1m]' })).toEqual({})
  })

  it('takes hyperparameters as a JSON object of at most 4 KB, empty text as none', () => {
    expect(parseHyperparameters('')).toEqual({ value: {} })
    expect(parseHyperparameters('{"lr": 0.1}')).toEqual({ value: { lr: 0.1 } })
    expect(parseHyperparameters('[1]')).toEqual({ problem: 'A JSON object, as {"lr": 0.001}.' })
    expect(parseHyperparameters('{lr')).toEqual({ problem: 'Not valid JSON.' })
    expect(parseHyperparameters(JSON.stringify({ k: 'x'.repeat(5000) }))).toHaveProperty('problem')
  })
})

describe('naming and describing types', () => {
  it('describes a built-in type by what it does and an org\'s own by its version and author', () => {
    expect(describeType(CATALOG[1])).toMatch(/tunes the knobs/)
    expect(describeType(CATALOG.find(t => t.id === 'at-lr')!)).toBe("Your org's own researcher type, made by Ada.")
  })

  it('reads an id as its name, and an id the catalog does not hold as itself', () => {
    expect(typeName('data-search', CATALOG)).toBe('Data search')
    expect(typeName('at-gone', CATALOG)).toBe('at-gone')
  })

  it('offers researcher types that are not archived, built-in first', () => {
    const archived = agentType('at-old', 'Old', 'researcher', { archived_at: '2026-02-01T00:00:00Z' })
    const ids = researcherTypes([CATALOG[7], archived, ...CATALOG.slice(0, 7)]).map(t => t.id)
    expect(ids).toEqual(['generalist', 'hyperparameter-search', 'architecture-search', 'data-search', 'at-lr'])
  })
})
