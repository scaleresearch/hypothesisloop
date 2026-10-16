import { describe, expect, it } from 'vitest'
import { diffLines, foldUnchanged } from '@/lib/diff'

describe('diffLines', () => {
  it('keeps common lines and marks the ones taken out and put in, with their line numbers', () => {
    expect(diffLines('a\nb\nc', 'a\nx\nc\nd')).toEqual([
      { kind: 'same', text: 'a', before: 1, after: 1 },
      { kind: 'del', text: 'b', before: 2 },
      { kind: 'add', text: 'x', after: 2 },
      { kind: 'same', text: 'c', before: 3, after: 3 },
      { kind: 'add', text: 'd', after: 4 },
    ])
  })

  it('reads two equal texts as unchanged', () => {
    expect(diffLines('a\nb', 'a\nb').every(l => l.kind === 'same')).toBe(true)
  })
})

describe('foldUnchanged', () => {
  it('folds a long unchanged stretch, keeping context around the change', () => {
    const before = Array.from({ length: 20 }, (_, i) => `line ${i}`).join('\n')
    const after = before.replace('line 10', 'line ten')
    const hunks = foldUnchanged(diffLines(before, after), 2)
    expect(hunks.map(h => [h.kind, h.lines.length])).toEqual([['skipped', 8], ['lines', 6], ['skipped', 7]])
  })

  it('leaves a short stretch as it is', () => {
    expect(foldUnchanged(diffLines('a\nb\nc', 'a\nB\nc'), 3)).toHaveLength(1)
  })
})
