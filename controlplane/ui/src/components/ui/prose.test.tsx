import { describe, expect, it } from 'vitest'
import { parseProse } from './prose'

describe('parseProse', () => {
  it('reads an all-caps line as a heading and keeps its bracketed note', () => {
    const blocks = parseProse('FINAL SUMMARY (coordinator; closed early)\nBEST RESULT AGAINST THE CONTROL\n- Control: 0.825\n- Top: 0.883\n1. tanh\n2. L2')
    expect(blocks).toEqual([
      { kind: 'heading', text: 'Final summary', note: 'coordinator; closed early' },
      { kind: 'heading', text: 'Best result against the control', note: undefined },
      { kind: 'list', ordered: false, items: ['Control: 0.825', 'Top: 0.883'] },
      { kind: 'list', ordered: true, items: ['tanh', 'L2'] },
    ])
  })

  it('keeps a sentence that starts with an acronym as text', () => {
    expect(parseProse('LR=0.1, EPOCHS=100 did better.')).toEqual([{ kind: 'paragraph', lines: ['LR=0.1, EPOCHS=100 did better.'] }])
  })

  it('reads a lone word in capitals as a heading', () => {
    expect(parseProse('OBJECTIVE\nMaximize it.')).toEqual([{ kind: 'heading', text: 'Objective', note: undefined }, { kind: 'paragraph', lines: ['Maximize it.'] }])
    expect(parseProse('NOTE')).toEqual([{ kind: 'paragraph', lines: ['NOTE'] }])
  })

  it('splits paragraphs at a blank line', () => {
    expect(parseProse('one\ntwo\n\nthree')).toEqual([{ kind: 'paragraph', lines: ['one', 'two'] }, { kind: 'paragraph', lines: ['three'] }])
  })
})
