import { describe, expect, it } from 'vitest'
import { inheritedModel } from '@/lib/models'

describe('the model an agent naming none runs on', () => {
  it("is its type's, else a researcher's coordinator's, else the platform's default", () => {
    expect(inheritedModel('openrouter/moonshotai/kimi-k2', true, 'anthropic/claude-opus-5-5')).toEqual({ inherited: 'openrouter/moonshotai/kimi-k2', inheritedFrom: "Type's model" })
    expect(inheritedModel('openrouter/moonshotai/kimi-k2', false, 'anthropic/claude-opus-5-5')).toEqual({ inherited: 'openrouter/moonshotai/kimi-k2', inheritedFrom: "Type's model" })
    expect(inheritedModel(undefined, true, 'anthropic/claude-opus-5-5')).toEqual({ inherited: 'anthropic/claude-opus-5-5', inheritedFrom: "Coordinator's model" })
    expect(inheritedModel('', false, 'anthropic/claude-opus-5-5')).toEqual({})
  })
})
