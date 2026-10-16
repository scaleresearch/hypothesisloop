import { describe, expect, it } from 'vitest'
import { BRIEF_POINTS, covers, withPoint } from '@/lib/brief-points'

const win = BRIEF_POINTS.find(p => p.label === 'Win')!

describe('a point of the brief', () => {
  it('is added as a line of its own, after the text or as its first line', () => {
    expect(withPoint('', win)).toEqual({ text: 'Win: ', caret: 5 })
    expect(withPoint('Does Lion beat AdamW?', win)).toEqual({ text: 'Does Lion beat AdamW?\nWin: ', caret: 27 })
    expect(withPoint('Goal\n', win).text).toBe('Goal\nWin: ')
  })

  it('is gone to, never added twice, when the text has its line', () => {
    const text = 'Goal\nwin: 2%\nFixed: eval.py'
    expect(withPoint(text, win)).toEqual({ text, caret: 12 })
  })

  it('counts as stated only with something after its label', () => {
    expect(covers('Goal\nWin: ', win)).toBe(false)
    expect(covers('Goal\n  Win: 2% over 3 seeds', win)).toBe(true)
    expect(covers('Winning: yes', win)).toBe(false)
  })
})
