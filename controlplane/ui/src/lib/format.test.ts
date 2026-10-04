import { describe, expect, it } from 'vitest'
import { headline } from './format'

describe('headline', () => {
  it('keeps the first sentence without its full stop', () => {
    expect(headline('Split the fields. Then code each.')).toBe('Split the fields')
  })

  it('cuts a long first sentence at a word', () => {
    const label = headline('word '.repeat(40), 20)
    expect(label).toBe('word word word word…')
  })

  it('leaves a short text whole', () => {
    expect(headline('Try xor-delta on 1.5x data')).toBe('Try xor-delta on 1.5x data')
  })
})
